package vfs_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// A NoFill reader serves what the chunk cache holds and never adds to it or prefetches:
// the stats and search readers, which must leave the LRU alone (I-196) yet read the
// system pin's chunks from the node's disk when every peer is away (I-373).
//
// Invariant: I-373, I-196
func TestReader_NoFill_ConsultsCacheNeverFills(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	data := testharness.Pattern(2*int(testharness.ChunkSize)+11, 4)
	writeFile(t, fs, "/seg", data)
	must(t, f.Flusher().FlushAll(ctx))
	c, err := chunk.OpenCache(sysfs.OS{}, filepath.Join(t.TempDir(), "cache"), 1<<30, f.Clk)
	must(t, err)
	t.Cleanup(c.Drain)
	in := f.Inode(f.InodeOf(fs, "/seg"))
	read := func(rd *vfs.Reader) ([]byte, error) {
		buf := make([]byte, in.Size)
		n, err := rd.ReadAt(ctx, in, buf, 0)
		return buf[:n], err
	}
	nofill := vfs.NewReader(f.FSM, f, f, c, nil)
	defer nofill.Close()
	nofill.NoFill = true
	got, err := read(nofill)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a NoFill read: %v", err)
	}
	if c.Used() != 0 {
		t.Fatalf("a NoFill read put %d bytes in the cache", c.Used())
	}
	filler := vfs.NewReader(f.FSM, f, f, c, nil)
	defer filler.Close()
	if _, err := read(filler); err != nil {
		t.Fatal(err)
	}
	c.Drain()
	for _, n := range []meta.NodeIdx{1, 2, 3} {
		f.SetDown(n, true)
	}
	got, err = read(nofill)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a NoFill read of cached chunks with every peer dead: %v", err)
	}
}
