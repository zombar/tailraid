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
