package repair_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var ctx = context.Background()

// flushedChunk writes one chunk through the fabric and returns its metadata.
func flushedChunk(t *testing.T, f *testharness.Fabric) meta.Chunk {
	t.Helper()
	ino := f.Create("a")
	if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), 3)); err != nil {
		t.Fatal(err)
	}
	if err := f.Flusher().FlushInode(ctx, ino); err != nil {
		t.Fatal(err)
	}
	c, ok := f.FSM.GetChunk(f.Inode(ino).Chunks[0])
	if !ok {
		t.Fatal("no chunk after flush")
	}
	return c
}

// The background repairer returns when its context is cancelled, leaving no goroutine
// behind (goleak checks at exit).
func TestRepairer_RunStopsOnCancel(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, repair.DefaultConfig(), nil)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(rctx, func() bool { return true }); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A scrub decodes from the shards it fetched and only falls back to the reader when
// they do not decode to the chunk's hash: one corrupt slot of 2+1 costs the three GETs of
// the scrub plus the k the repair needs, never a second full fetch.
//
// Invariant: I-15
func TestScrub_DecodesFromFetchedShards(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	c := flushedChunk(t, f)
	victim := c.Placement[1]
	f.CorruptShard(victim, chunk.Key(c.Hash, c.K, c.M, 1))
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, repair.DefaultConfig(), nil)
	before := 0
	for n := meta.NodeIdx(1); n <= 3; n++ {
		before += f.Gets(n)
	}
	st, err := r.Scrub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ScrubBad != 1 {
		t.Fatalf("scrub found %d bad slots, want 1 (%+v)", st.ScrubBad, st)
	}
	gets := -before
	for n := meta.NodeIdx(1); n <= 3; n++ {
		gets += f.Gets(n)
	}
	// the scrub fetches each of the 3 slots once; the repair's own read needs k, or k+1
	// when its rank order tries the quarantined slot first. Fetching the chunk again
	// inside the scrub, as it used to, would add k more.
	if lo, hi := 3+int(c.K), 3+int(c.K)+1; gets < lo || gets > hi {
		t.Fatalf("%d GETs for a scrub and one repair, want %d..%d", gets, lo, hi)
	}
	// the replaced shard verifies again
	if _, err := f.Stores[victim].Get(chunk.Key(c.Hash, c.K, c.M, 1)); err != nil {
		t.Fatalf("repaired slot: %v", err)
	}
}

// GC collects a tombstoned chunk strictly after grace: at grace−1 and at exactly grace
// it stays, at grace+1 it goes.
//
// Invariant: I-13
func TestGC_GraceBoundary(t *testing.T) {
	const grace = 10 * time.Minute
	for _, tc := range []struct {
		name string
		age  time.Duration
		want int
	}{{"grace-1", grace - time.Nanosecond, 0}, {"grace", grace, 0}, {"grace+1", grace + time.Nanosecond, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			flushedChunk(t, f)
			tomb := f.Clk.Now()
			f.MustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "a", Now: tomb.UnixNano()}})
			f.Clk.Set(tomb.Add(tc.age))
			gc := repair.NewGC(f.FSM, f, f, f, f.Clk, grace, nil)
			res, err := gc.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if res.Victims != tc.want {
				t.Fatalf("victims %d, want %d", res.Victims, tc.want)
			}
		})
	}
}
