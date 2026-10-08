package vfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/gurgeh/tailraid/internal/chunk"
	"github.com/gurgeh/tailraid/internal/meta"
)

// writeAndFlush creates a file holding data and flushes it through the real flusher.
func writeAndFlush(t *testing.T, f *fabric, name string, data []byte) meta.Ino {
	t.Helper()
	ino := f.create(name)
	must(t, f.ov.Write(ino, 0, 0, data))
	if err := f.flusher().FlushInode(context.Background(), ino); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return ino
}

// Lifecycle: L-66, L-74
func TestReader_ReadsWithOneNodeDown_AndFromCache(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	data := pattern(int(fabCS), 1)
	ino := writeAndFlush(t, f, "a", data)
	in := f.inode(ino)
	h := in.Chunks[0]
	c, _ := f.fsm.GetChunk(h)

	// a reader on a gateway with a cold cache and one holder down decodes from k shards
	f.setDown(c.Placement[0], true)
	cold := f.readerNoCache()
	got := make([]byte, len(data))
	if _, err := cold.ReadAt(context.Background(), in, got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", firstDiff(got, data))
	}
	// cache hit on the issuing gateway: every node down, still readable (L-66)
	for _, n := range c.Placement {
		f.setDown(n, true)
	}
	rd := f.reader()
	got2 := make([]byte, len(data))
	if _, err := rd.ReadAt(context.Background(), in, got2, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("cache read: %v", err)
	}
	if !bytes.Equal(got2, data) {
		t.Fatal("cache content differs")
	}
}

// Lifecycle: L-75
func TestReader_FewerThanKLive_EIO(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	data := pattern(int(fabCS), 2)
	ino := writeAndFlush(t, f, "a", data)
	in := f.inode(ino)
	c, _ := f.fsm.GetChunk(in.Chunks[0])
	f.setDown(c.Placement[0], true)
	f.setDown(c.Placement[1], true)
	rd := f.readerNoCache()
	_, err := rd.ReadChunk(context.Background(), in.Chunks[0])
	if !errors.Is(err, ErrIO) {
		t.Fatalf("got %v, want ErrIO", err)
	}
}

// Lifecycle: L-68, L-105
func TestReader_CorruptShard_DecodesFromOthers_AndReports(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	data := pattern(int(fabCS), 3)
	ino := writeAndFlush(t, f, "a", data)
	in := f.inode(ino)
	h := in.Chunks[0]
	c, _ := f.fsm.GetChunk(h)
	// corrupt the slot held by this gateway: Rank puts self first, so it is always read
	slot := -1
	for i, n := range c.Placement {
		if n == f.self {
			slot = i
		}
	}
	if slot < 0 {
		t.Fatalf("self not in placement %v", c.Placement)
	}
	f.corruptShard(f.self, chunk.Key(h, c.K, c.M, slot))
	rd := f.readerNoCache()
	var reported []chunk.ShardKey
	rd.BadSlot = func(key chunk.ShardKey, _ meta.NodeIdx) { reported = append(reported, key) }
	got, err := rd.ReadChunk(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("decoded content differs")
	}
	if len(reported) != 1 || reported[0] != chunk.Key(h, c.K, c.M, slot) {
		t.Fatalf("bad slot report = %v", reported)
	}
}

// Lifecycle: L-69
func TestReader_DecodedHashMismatch_EIO(t *testing.T) {
	f := newFabric(t, 3, 1, 1)
	// register a chunk entry whose hash does not match the bytes its shards hold
	data := pattern(1000, 4)
	wrong := sha256.Sum256([]byte("not the data"))
	key0, key1 := chunk.Key(wrong, 1, 1, 0), chunk.Key(wrong, 1, 1, 1)
	ctx := context.Background()
	must(t, f.Put(ctx, 1, key0, data))
	must(t, f.Put(ctx, 2, key1, data))
	ino := f.create("x")
	f.mustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, NewSize: 1000, Writes: []meta.ChunkWrite{{Idx: 0, Hash: wrong}},
		NewChunks: []meta.ChunkDesc{{Hash: wrong, Size: 1000, K: 1, M: 1, Placement: []meta.NodeIdx{1, 2}}}, Now: 2}})
	_, err := f.readerNoCache().ReadChunk(ctx, wrong)
	if !errors.Is(err, ErrIO) {
		t.Fatalf("got %v, want ErrIO", err)
	}
}

// Lifecycle: L-71
func TestReader_ReadAt_HolesAndShortChunks(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	ino := f.create("a")
	cs := uint64(fabCS)
	// write only in chunk 2 (bytes 2cs+10..2cs+20) → chunks 0,1 are holes, chunk 2 is short
	must(t, f.ov.Write(ino, 0, 2*cs+10, []byte("0123456789")))
	must(t, f.flusher().FlushInode(context.Background(), ino))
	in := f.inode(ino)
	if len(in.Chunks) != 3 || in.Chunks[0] != meta.HashZero || in.Chunks[1] != meta.HashZero || in.Chunks[2] == meta.HashZero {
		t.Fatalf("chunks %v", in.Chunks)
	}
	c, _ := f.fsm.GetChunk(in.Chunks[2])
	if c.Size != 20 || c.K != 1 { // 20-byte chunk → small-chunk policy k=1
		t.Fatalf("chunk entry %+v", c)
	}
	// truncate up through the FSM: the short chunk is zero-filled on read
	f.mustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, BaseVersion: in.Version, NewSize: 3 * cs, Now: 3}})
	in = f.inode(ino)
	want := make([]byte, 3*cs)
	copy(want[2*cs+10:], "0123456789")
	got := make([]byte, 3*cs)
	rd := f.reader()
	n, err := rd.ReadAt(context.Background(), in, got, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("n=%d differs at %d", n, firstDiff(got, want))
	}
	// reads past EOF and short reads
	if n, err := rd.ReadAt(context.Background(), in, got, int64(3*cs)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF: %d %v", n, err)
	}
	if n, err := rd.ReadAt(context.Background(), in, got[:100], int64(3*cs-50)); n != 50 || !errors.Is(err, io.EOF) {
		t.Fatalf("read across EOF: %d %v", n, err)
	}
}
