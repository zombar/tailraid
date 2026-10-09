package vfs_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// A 64-chunk plan flushed with two chunk workers never holds more than two chunks and
// their shards at once, and holds nothing by the time the Commit is proposed or after
// the flush: the bytes of a chunk live only while it is being decided (docs/04 §9).
// Invariant: I-78
func TestFlusher_Buffers_BoundedByChunkWorkers(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := int(testharness.ChunkSize)
	ino := f.Create("a")
	data := testharness.Pattern(64*cs, 31)
	must(t, f.Ov.Write(ino, 0, 0, data))
	fl := f.Flusher(func(c *vfs.Config) { c.ChunkWorkers = 2 })
	atCommit := uint64(1)
	fl.Hooks.BeforeCommit = func() { atCommit, _ = fl.Buffers() }
	must(t, fl.FlushInode(ctx, ino))
	cur, peak := fl.Buffers()
	limit := uint64(2 * (cs + 3*ec.ShardLen(cs, 2)))
	if peak == 0 || peak > limit {
		t.Fatalf("peak %d bytes held, limit %d", peak, limit)
	}
	if cur != 0 || atCommit != 0 {
		t.Fatalf("held after the flush %d, at the commit %d", cur, atCommit)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
}

// A plan larger than FileDirtyHi bytes of chunks is committed in batches: each flush
// commits the lowest-indexed chunks that fit, the size never runs ahead of committed
// data, the rest stays dirty and readable, and the following flushes commit it in order.
// Invariant: I-80
// Lifecycle: L-191
func TestFlusher_BatchCommit_ResidualStaysDirty(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	data := testharness.Pattern(int(10*cs), 32)
	must(t, f.Ov.Write(ino, 0, 0, data))
	fl := f.Flusher(func(c *vfs.Config) { c.FileDirtyHi = 4 * cs })
	for _, want := range []struct {
		version       uint64
		size          uint64
		chunks        int
		residualBytes uint64
	}{{1, 4 * cs, 4, 6 * cs}, {2, 8 * cs, 8, 2 * cs}, {3, 10 * cs, 10, 0}} {
		must(t, fl.FlushInode(ctx, ino))
		in := f.Inode(ino)
		if in.Version != want.version || in.Size != want.size || len(in.Chunks) != want.chunks {
			t.Fatalf("after batch %d: version %d size %d chunks %d, want %+v", want.version, in.Version, in.Size, len(in.Chunks), want)
		}
		d := f.Ov.Dirty()
		switch {
		case want.residualBytes == 0 && len(d) != 0:
			t.Fatalf("after the last batch the overlay still holds %+v", d)
		case want.residualBytes != 0 && (len(d) != 1 || d[0].DirtyBytes != want.residualBytes || d[0].Flushing):
			t.Fatalf("after batch %d: residual %+v, want %d bytes", want.version, d, want.residualBytes)
		}
		if got := f.ReadFile(ino); !bytes.Equal(got, data) {
			t.Fatalf("after batch %d: content differs at %d", want.version, testharness.FirstDiff(got, data))
		}
	}
}

// A Commit that conflicts is retried against the newer base by re-assembling the plan's
// chunks from the WAL: nothing is held across the attempts, and the retry still carries
// every dirty byte while keeping the other gateway's append.
// Invariant: I-35, I-78
func TestFlusher_ConflictRetry_ReassemblesFromWAL(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	base := testharness.Pattern(int(2*cs), 8)
	must(t, f.Ov.Write(ino, 0, 0, base))
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	mine := testharness.Pattern(int(cs), 9)
	must(t, f.Ov.Write(ino, in.Size, 0, mine))
	other := testharness.Pattern(int(cs), 10)
	oh := sha256Of(other)
	fl := f.Flusher()
	attempts := 0
	var held []uint64
	fl.Hooks.BeforeCommit = func() {
		attempts++
		cur, _ := fl.Buffers()
		held = append(held, cur)
		if attempts > 1 {
			return
		}
		must(t, f.Put(ctx, 1, chunk.Key(oh, 2, 1, 0), other[:cs/2]))
		f.MustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, BaseVersion: in.Version, NewSize: 3 * cs,
			Writes: []meta.ChunkWrite{{Idx: 2, Hash: oh}}, NewChunks: []meta.ChunkDesc{{Hash: oh, Size: uint32(cs), K: 2, M: 1, Placement: []meta.NodeIdx{1, 2, 3}}}, Now: 7}})
	}
	must(t, fl.FlushInode(ctx, ino))
	if attempts != 2 {
		t.Fatalf("%d commit attempts, want 2", attempts)
	}
	for i, h := range held {
		if h != 0 {
			t.Fatalf("attempt %d proposed while holding %d bytes", i+1, h)
		}
	}
	final := f.Inode(ino)
	if final.Size != 3*cs || len(final.Chunks) != 3 || final.Chunks[0] != sha256Of(mine) || final.Chunks[2] != oh || final.Chunks[1] != in.Chunks[1] {
		t.Fatalf("final %+v", final)
	}
	if len(f.Ov.Dirty()) != 0 {
		t.Fatal("extents left dirty")
	}
}

// A PUT that is still in flight when its slot is re-placed after the deadline is
// cancelled and awaited before the chunk is decided: the flush returns with the straggler
// gone and nothing held, and the stalled node is not in the placement.
// Invariant: I-83
// Lifecycle: L-190
func TestFlusher_StragglerPut_CancelledAfterReplace(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 7)
	must(t, f.Ov.Write(ino, 0, 0, data))
	h := sha256Of(data)
	cands := meta.Candidates(f.FSM.Nodes(), func(meta.NodeIdx) bool { return true })
	victim := meta.Place(h, cands, 3)[2]
	entered, release := f.StallPut(victim)
	defer release()
	fl := f.Flusher(func(c *vfs.Config) { c.PutDeadline = time.Second })
	done := make(chan error, 1)
	go func() { done <- fl.FlushInode(ctx, ino) }()
	<-entered
	var err error
	// the deadline is on the fake clock, advanced until the stalled PUT has been cancelled
	// (the first round ending); the re-placed PUT then runs under a deadline nothing
	// advances, so it never runs out of time however loaded the machine is
	testharness.Eventually(t, func() bool {
		if f.CancelledPuts(victim) == 0 {
			f.Clk.Advance(time.Second / 4)
		}
		select {
		case err = <-done:
			return true
		default:
			return false
		}
	}, "flush past the PUT deadline")
	must(t, err)
	if n := f.CancelledPuts(victim); n != 1 {
		t.Fatalf("%d cancelled PUTs to the stalled node, want 1", n)
	}
	if cur, _ := fl.Buffers(); cur != 0 {
		t.Fatalf("%d bytes still held after the flush", cur)
	}
	c, ok := f.FSM.GetChunk(h)
	if !ok {
		t.Fatal("chunk not committed")
	}
	for _, s := range c.Placement {
		if s == 0 || s == victim {
			t.Fatalf("placement %v should be full and avoid the stalled node %d", c.Placement, victim)
		}
	}
}

// When every slot fails because the holders are full, the flush error says so, so the
// client sees ENOSPC rather than EIO.
// Invariant: I-79
func TestFlusher_FullCluster_ErrorWrapsErrFull(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	for n := meta.NodeIdx(1); n <= 3; n++ {
		f.SetFull(n, true)
	}
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), 3)))
	err := f.Flusher().FlushInode(ctx, ino)
	if !errors.Is(err, vfs.ErrTooFewAcks) || !errors.Is(err, chunk.ErrFull) {
		t.Fatalf("err = %v, want ErrTooFewAcks wrapping chunk.ErrFull", err)
	}
	if d := f.Ov.Dirty(); len(d) != 1 || d[0].Flushing {
		t.Fatalf("dirty state after the failed flush: %+v", d)
	}
}

// A flush does not populate the read cache; the first read after it does.
// Invariant: I-78
func TestFlusher_DoesNotFillCache(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 4)
	must(t, f.Ov.Write(ino, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, ino))
	h := sha256Of(data)
	if _, ok := f.Cache.Get(h); ok {
		t.Fatal("the flush filled the read cache")
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if _, ok := f.Cache.Get(h); !ok {
		t.Fatal("the read did not fill the cache")
	}
}
