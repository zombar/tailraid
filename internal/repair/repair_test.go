package repair_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	if _, err := f.StoreOf(victim).Get(chunk.Key(c.Hash, c.K, c.M, 1)); err != nil {
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

// fanoutFabric is a five-host 3+1 fabric with n flushed chunks whose most-held target is
// then taken down, so every chunk that held a slot there has one to re-place onto the
// one spare; PUTs to every live host stall until release is called.
func fanoutFabric(t *testing.T, n int) (f *testharness.Fabric, inos []meta.Ino, victim meta.NodeIdx, release func()) {
	t.Helper()
	f = testharness.NewFabric(t, 5, 3, 1)
	held := map[meta.NodeIdx]int{}
	for i := range n {
		ino := f.Create(fmt.Sprintf("f%d", i))
		if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), byte(i+1))); err != nil {
			t.Fatal(err)
		}
		if err := f.Flusher().FlushInode(ctx, ino); err != nil {
			t.Fatal(err)
		}
		c, ok := f.FSM.GetChunk(f.Inode(ino).Chunks[0])
		if !ok {
			t.Fatal("no chunk after flush")
		}
		for _, s := range c.Placement {
			held[s]++
		}
		inos = append(inos, ino)
	}
	for s, k := range held {
		if k > held[victim] {
			victim = s
		}
	}
	f.SetDown(victim, true)
	var releases []func()
	for h := meta.NodeIdx(1); h <= 5; h++ {
		if f.Target(h) == victim {
			continue
		}
		_, rel := f.StallPut(h)
		releases = append(releases, rel)
	}
	return f, inos, victim, func() {
		for _, rel := range releases {
			rel()
		}
	}
}

// slotsOn counts the slots of the chunks of inos placed on target.
func slotsOn(f *testharness.Fabric, inos []meta.Ino, target meta.NodeIdx) int {
	n := 0
	for _, ino := range inos {
		c, _ := f.FSM.GetChunk(f.Inode(ino).Chunks[0])
		for _, s := range c.Placement {
			if s == target {
				n++
			}
		}
	}
	return n
}

func fanoutCfg(workers int) repair.Config {
	cfg := repair.DefaultConfig()
	cfg.ScrubPeriod, cfg.RateBytes, cfg.Workers = 0, 0, workers
	return cfg
}

// A pass works on at most Workers chunks at once: with every PUT stalled, the stall's
// high-water mark reaches Workers and never passes it, Progress reports the pass while
// it runs and not after, and the statistics equal a serial pass's over the same chunks.
//
// Invariant: I-174
func TestRepair_FanOut_Bounded(t *testing.T) {
	const workers = 4
	f, inos, victim, release := fanoutFabric(t, 12)
	if slotsOn(f, inos, victim) < workers {
		t.Fatalf("setup: the dead target held %d slots, want at least %d", slotsOn(f, inos, victim), workers)
	}
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, fanoutCfg(workers), nil)
	done := make(chan repair.Stats, 1)
	go func() {
		st, err := r.Scan(ctx)
		if err != nil {
			t.Error(err)
		}
		done <- st
	}()
	testharness.Eventually(t, func() bool { return f.StallPeak() >= workers }, "the pass never had Workers PUTs in flight")
	p, running := r.Progress()
	if !running || p.Workers != workers || p.InFlight != workers || p.Total != 12 || p.Pass != 1 {
		t.Fatalf("progress mid-pass: %+v running %v", p, running)
	}
	release()
	st := <-done
	if f.StallPeak() != workers {
		t.Fatalf("%d PUTs in flight at peak, want %d", f.StallPeak(), workers)
	}
	if _, running := r.Progress(); running {
		t.Fatal("still running after the pass")
	}
	if n := slotsOn(f, inos, victim) + slotsOn(f, inos, 0); n != 0 {
		t.Fatalf("%d slots still on the dead target or holed after the pass", n)
	}
	// the same chunks one at a time: the same statistics
	f1, _, _, release1 := fanoutFabric(t, 12)
	release1()
	st1, err := repair.New(f1.FSM, f1, f1, f1, f1.ReaderNoCache(), f1.Clk, fanoutCfg(1), nil).Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st != st1 {
		t.Fatalf("fan-out pass %+v, serial pass %+v", st, st1)
	}
}

// A cancelled pass starts no more chunks and returns once the ones in flight end, with
// nothing committed by the PUTs it cut short.
//
// Invariant: I-174
func TestRepair_FanOut_CancelDrains(t *testing.T) {
	f, inos, victim, release := fanoutFabric(t, 12)
	defer release()
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, fanoutCfg(4), nil)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := r.Scan(cctx); done <- err }()
	testharness.Eventually(t, func() bool { return f.StallPeak() >= 1 }, "no PUT reached the stall")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled pass returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Scan did not return after cancel")
	}
	if _, running := r.Progress(); running {
		t.Fatal("still running after the cancel")
	}
	if slotsOn(f, inos, victim) == 0 {
		t.Fatal("a cancelled pass re-placed every chunk")
	}
}

// A host held slow takes no refill: with every spare but one held, the dead slot's
// refill lands on that one, whichever spare it is (so the test does not pass by the
// refill's own choice); and a held host's own slot stays where it is, a good slot, since
// it answers (I-371).
//
// Invariant: I-371
// Lifecycle: L-222
func TestRepair_SlowHostNotADestination(t *testing.T) {
	probe := testharness.NewFabric(t, 6, 2, 1)
	c := flushedChunk(t, probe)
	var spares []meta.NodeIdx
	for _, cand := range meta.Candidates(probe.FSM.Nodes(), func(meta.NodeIdx) bool { return true }) {
		if !slices.Contains(c.Placement, cand.Idx) {
			spares = append(spares, cand.Idx)
		}
	}
	if len(spares) < 2 {
		t.Fatalf("setup: %d spare targets, want at least 2", len(spares))
	}
	for _, free := range spares {
		t.Run(fmt.Sprintf("free %d", free), func(t *testing.T) {
			f := testharness.NewFabric(t, 6, 2, 1)
			c := flushedChunk(t, f) // the same bytes on the same fabric: the same placement
			victim, keptHeld := c.Placement[0], c.Placement[1]
			held := map[meta.NodeIdx]bool{keptHeld: true}
			for _, s := range spares {
				if s != free {
					held[s] = true
				}
			}
			f.SetDown(victim, true)
			r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, fanoutCfg(1), nil)
			r.Held = func(n meta.NodeIdx) bool { return held[n] }
			if _, err := r.Scan(ctx); err != nil {
				t.Fatal(err)
			}
			got, _ := f.FSM.GetChunk(c.Hash)
			if got.Placement[0] != free {
				t.Fatalf("the dead slot went to %d, want the one unheld spare %d (placement %v)", got.Placement[0], free, got.Placement)
			}
			if got.Placement[1] != keptHeld {
				t.Fatalf("the held host's own slot moved: %v", got.Placement)
			}
		})
	}
}
