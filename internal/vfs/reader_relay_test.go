package vfs_test

import (
	"bytes"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// Readahead to a relayed holder fetches at most one chunk: a sequential READ prefetches
// the next chunk whatever its holders, and stops there when the chunk after would need
// a relayed holder (fewer than k of its live holders on a direct path).
// Invariant: I-329
func TestReader_Readahead_RelayedHolderOneChunk(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(32*cs, 5)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	k := 2
	// every host but this one behind a relay: a chunk with two slots off this host
	// needs a relayed holder
	self := f.Self()
	for _, n := range f.FSM.Nodes() {
		if n.Kind == meta.KindDevice && n.Idx != self {
			f.SetClass(n.Idx, meta.PathRelay)
		}
	}
	relayed := func(h meta.Hash) bool {
		c, _ := f.FSM.GetChunk(h)
		direct := 0
		for _, n := range c.Placement {
			if n != 0 && f.Class(n) != meta.PathRelay {
				direct++
			}
		}
		return direct < k
	}
	// a READ of chunk idx where idx+1 is relayed (prefetched anyway: one chunk) and idx+2
	// is relayed (not prefetched)
	idx := -1
	for i := 0; i+2 < len(in.Chunks); i++ {
		if relayed(in.Chunks[i+1]) && relayed(in.Chunks[i+2]) {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("no two consecutive chunks that need a relayed holder")
	}
	rd := f.Reader()
	drained(t, rd)
	got := readChunk(t, rd, in, idx)
	if !bytes.Equal(got, data[idx*cs:(idx+1)*cs]) {
		t.Fatalf("chunk %d differs", idx)
	}
	testharness.Eventually(t, func() bool { return cached(f, in.Chunks[idx+1]) && rd.Prefetching() == 0 }, "the next chunk prefetched")
	if cached(f, in.Chunks[idx+2]) {
		t.Fatalf("chunk %d was prefetched through a relayed holder", idx+2)
	}
	if fetched, _ := rd.ReadaheadStats(); fetched != 1 {
		t.Fatalf("readahead fetched %d chunks, want 1", fetched)
	}
	// every path direct again: the window is the full depth
	for _, n := range f.FSM.Nodes() {
		if n.Kind == meta.KindDevice {
			f.SetClass(n.Idx, meta.PathLAN)
		}
	}
	rd2 := f.Reader()
	drained(t, rd2)
	readChunk(t, rd2, in, idx)
	depth := vfs.ReadaheadDepth(uint32(cs))
	testharness.Eventually(t, func() bool {
		for j := 1; j <= depth && idx+j < len(in.Chunks); j++ {
			if !cached(f, in.Chunks[idx+j]) {
				return false
			}
		}
		return rd2.Prefetching() == 0
	}, "the full window prefetched on direct paths")
}
