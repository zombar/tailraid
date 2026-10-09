package vfs_test

import (
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A truncate followed by a far seek commits the written chunk and rewrites the base's
// cut-away chunks as holes; the indices in between, past the base list, are holes already
// and cost no ChunkWrite. Before the fix every index up to the seek was in the touched
// set, so a seek to 1 TiB produced a Commit of a million writes.
//
// Invariant: I-80
func TestFlusher_TruncateThenFarSeek_TouchesOnlyRealChunks(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, testharness.Pattern(int(cs), 1))) // chunk 0 in the base
	must(t, f.Flusher().FlushInode(ctx, ino))
	base := f.Inode(ino)
	if len(base.Chunks) != 1 {
		t.Fatalf("base chunks %v", base.Chunks)
	}
	// truncate to zero, then one byte far away: 1000 chunk indices in between
	must(t, f.Ov.Truncate(ino, base.Size, 0))
	must(t, f.Ov.Write(ino, 0, 1000*cs, []byte{1}))
	plan, ok := f.Ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("no plan")
	}
	touched := f.Flusher().Touched(plan, base)
	if len(touched) != 2 || touched[0] != 0 || touched[1] != 1000 {
		t.Fatalf("touched %v, want [0 1000]: chunk 0 becomes a hole, chunk 1000 is written, nothing in between", touched)
	}
	f.Ov.AbortFlush(plan) // the snapshot was only for the touched set; the flush below does the work
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	if len(in.Chunks) != 1001 || in.Chunks[0] != meta.HashZero || in.Chunks[500] != meta.HashZero || in.Chunks[1000] == meta.HashZero {
		t.Fatalf("chunks after flush: len %d, [0]=%x [500]=%x [1000]=%x", len(in.Chunks), in.Chunks[0][:4], in.Chunks[500][:4], in.Chunks[1000][:4])
	}
}
