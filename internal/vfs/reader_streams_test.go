package vfs_test

import (
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// Two interleaved streams of one file each keep their readahead: a player reading the
// file from its start and a request for its tail (where an MP4's index often sits) no
// longer cancel each other's prefetches, and the start stream's next READ stays
// sequential and joins its prefetch.
// Invariant: I-203
func TestReader_Readahead_TwoStreamsKeepTheirPrefetches(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 31)
	in := f.Inode(writeAndFlush(t, f, "v", data))
	rd := f.Reader()
	drained(t, rd)
	readChunk(t, rd, in, 0)  // stream A: prefetches 1..ReadAhead
	readChunk(t, rd, in, 14) // stream B: the tail
	readChunk(t, rd, in, 15) // B goes on: not a seek of A
	if n := rd.ReadaheadCancelled(); n != 0 {
		t.Fatalf("the tail stream cancelled %d of the start stream's prefetches", n)
	}
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	before, _ := rd.ReadaheadStats()
	readChunk(t, rd, in, 1) // A goes on: served by its prefetch, and slides its window
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	after, joined := rd.ReadaheadStats()
	if joined == 0 || after <= before {
		t.Fatalf("stream A lost its readahead: fetched %d → %d, joined %d", before, after, joined)
	}
	if !cached(f, in.Chunks[vfs.ReadAhead+1]) {
		t.Fatalf("stream A's window did not slide to chunk %d", vfs.ReadAhead+1)
	}
}

// A client's concurrent READs reach the reader out of order: an NFS client keeps up to
// InFlight (16) READs of 1 MiB in flight (docs/07), and through the macOS mount a READ
// of chunk n+1 was served before the last READ of chunk n. A READ within ReadAhead
// chunks of a stream's frontier, either side, continues the stream and cancels nothing,
// and the frontier is the furthest chunk read, so a late READ of an earlier chunk
// prefetches nothing the stream has passed; a READ past the window is a seek as before.
// On the three-VM rig a sequential read of 64 chunks through the macOS mount was served
// 36 times by readahead with 14 prefetches cancelled (run 20261014-211704).
// Regression: I-203
func TestReader_Readahead_OutOfOrderReadsStayInSequence(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(32*cs, 16)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	g := &countGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	drained(t, rd)
	k := 2
	rd.Prefetch(in, 0) // chunks 1..ReadAhead in flight, every slot busy
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "prefetches in flight")
	within := []int{2, 1, 3, vfs.ReadAhead, 0, 2 * vfs.ReadAhead, vfs.ReadAhead} // out of order, the frontier's edge ahead (ReadAhead past 4), then behind (ReadAhead before 8)
	for _, idx := range within {
		rd.Prefetch(in, int64(idx*cs))
		if n := rd.ReadaheadCancelled(); n != 0 {
			t.Fatalf("a READ of chunk %d cancelled %d prefetches", idx, n)
		}
	}
	if n := rd.Prefetching(); n != vfs.ReadAhead {
		t.Fatalf("%d prefetches in flight, want the %d first ones", n, vfs.ReadAhead)
	}
	rd.Prefetch(in, int64(31*cs))                  // a second stream (the tail): takes the empty slot
	rd.Prefetch(in, int64((3*vfs.ReadAhead+1)*cs)) // past the first stream's window (frontier 2·ReadAhead) and before the tail's: a seek
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "the seek cancelled the first stream's prefetches")
	if n := rd.ReadaheadCancelled(); n != uint64(vfs.ReadAhead) {
		t.Fatalf("cancelled %d, want %d", n, vfs.ReadAhead)
	}
	close(g.release)
}
