package vfs_test

import (
	"bytes"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// An aligned full-chunk overwrite assembles without fetching the base chunk (no
// read-modify-write round trip); a partial overwrite still does.
// Lifecycle: L-7, L-30
func TestFlusher_FullCoverChunks_NoBaseFetch(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := int(testharness.ChunkSize)
	// a cache-less reader, so every base fetch is a shard GET the fabric counts
	fl := vfs.NewFlusher(f.Ov, f.FSM, f, f, f, f.ReaderNoCache(), nil, f.Clk, vfs.DefaultConfig(), nil)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, testharness.Pattern(2*cs, 1)))
	must(t, fl.FlushInode(ctx, ino))
	gets := func() int {
		n := 0
		for i := meta.NodeIdx(1); i <= 3; i++ {
			n += f.Gets(i)
		}
		return n
	}
	before := gets()
	must(t, f.Ov.Write(ino, uint64(2*cs), 0, testharness.Pattern(2*cs, 2))) // both chunks entirely dirty
	must(t, fl.FlushInode(ctx, ino))
	if got := gets(); got != before {
		t.Fatalf("full-cover flush fetched %d shards", got-before)
	}
	must(t, f.Ov.Write(ino, uint64(2*cs), uint64(cs+10), []byte("partial"))) // chunk 1 partly dirty: RMW
	must(t, fl.FlushInode(ctx, ino))
	if got := gets(); got <= before {
		t.Fatal("partial overwrite fetched no base shards")
	}
	want := testharness.Pattern(2*cs, 2)
	copy(want[cs+10:], "partial")
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, want))
	}
}
