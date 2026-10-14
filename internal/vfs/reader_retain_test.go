package vfs_test

import (
	"fmt"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
)

// A finished prefetch is held by the chunk cache alone: the readahead table forgets it
// once it completes, so a gateway that reads the heads of many files (thumbnails, tags,
// frame grabs) keeps no decoded chunk alive past the cache's bound.
// Invariant: I-310
func TestReader_Readahead_FinishedPrefetchNotRetained(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	rd := f.Reader()
	drained(t, rd)
	const files = 12
	for i := range files {
		data := testharness.Pattern(3*cs+cs/2, byte(i+1))
		in := f.Inode(writeAndFlush(t, f, fmt.Sprintf("f%d", i), data))
		readChunk(t, rd, in, 0) // the head only: the client never comes back
		testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches finished")
	}
	if fetched, _ := rd.ReadaheadStats(); fetched == 0 {
		t.Fatal("no prefetch ran: the test proves nothing")
	}
	if n := rd.PrefetchHeld(); n != 0 {
		t.Fatalf("readahead table still holds %d finished prefetches", n)
	}
}
