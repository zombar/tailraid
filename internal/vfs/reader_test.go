package vfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// writeAndFlush creates a file holding data and flushes it through the real flusher.
func writeAndFlush(t *testing.T, f *testharness.Fabric, name string, data []byte) meta.Ino {
	t.Helper()
	ino := f.Create(name)
	must(t, f.Ov.Write(ino, 0, 0, data))
	if err := f.Flusher().FlushInode(context.Background(), ino); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return ino
}

// A holder the prober reports dead is never dialled: it would only cost the dial or
// header timeout; the read takes k shards from the live holders.
// Lifecycle: L-131
func TestReader_SkipsDeadHolders(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 3)
	ino := writeAndFlush(t, f, "a", data)
	in := f.Inode(ino)
	c, _ := f.FSM.GetChunk(in.Chunks[0])
	// the dead holder is the one the fabric would rank first after self
	dead := meta.NodeIdx(0)
	for _, n := range c.Placement {
		if n != f.Self() && (dead == 0 || n < dead) {
			dead = n
		}
	}
	f.SetDown(dead, true)
	got := make([]byte, len(data))
	if _, err := f.ReaderNoCache().ReadAt(context.Background(), in, got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
	if n := f.Gets(dead); n != 0 {
		t.Fatalf("dead holder %d was dialled %d times", dead, n)
	}
	total := 0
	for _, n := range c.Placement {
		total += f.Gets(n)
	}
	if total != int(c.K) {
		t.Fatalf("%d GETs for a k=%d read", total, c.K)
	}
}

// flatPeers ranks nothing: every holder is equally close, so the slot order decides.
type flatPeers struct{ *testharness.Fabric }

func (p flatPeers) Rank(nodes []meta.NodeIdx) []meta.NodeIdx { return nodes }

// With every holder reachable and equally ranked the reader fetches exactly k shards:
// this host's own (free) and then data slots, so no parity shard crosses the network.
// Lifecycle: L-63
func TestReader_FetchFirstK(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 2)
	data := testharness.Pattern(int(testharness.ChunkSize), 4)
	ino := writeAndFlush(t, f, "a", data)
	in := f.Inode(ino)
	c, _ := f.FSM.GetChunk(in.Chunks[0])
	rd := vfs.NewReader(f.FSM, flatPeers{f}, f, nil, &ec.Pool{})
	got := make([]byte, len(data))
	if _, err := rd.ReadAt(context.Background(), in, got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
	want := WantGets(f, c)
	for i, n := range c.Placement {
		if g := f.Gets(n); g != want[n] {
			t.Fatalf("slot %d (node %d): %d GETs, want %d; placement %v", i, n, g, want[n], c.Placement)
		}
	}
}

// Lifecycle: L-66, L-74
func TestReader_ReadsWithOneNodeDown_AndFromCache(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 1)
	ino := writeAndFlush(t, f, "a", data)
	in := f.Inode(ino)
	h := in.Chunks[0]
	c, _ := f.FSM.GetChunk(h)

	// a reader on a gateway with a cold cache and one holder down decodes from k shards
	f.SetDown(c.Placement[0], true)
	cold := f.ReaderNoCache()
	got := make([]byte, len(data))
	if _, err := cold.ReadAt(context.Background(), in, got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
	// cache hit on the issuing gateway: a read fills the cache (the flush does not, I-78),
	// then with every node down the chunk is still readable (L-66)
	rd := f.Reader()
	warm := make([]byte, len(data))
	if _, err := rd.ReadAt(context.Background(), in, warm, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for _, n := range c.Placement {
		f.SetDown(n, true)
	}
	got2 := make([]byte, len(data))
	if _, err := rd.ReadAt(context.Background(), in, got2, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("cache read: %v", err)
	}
	if !bytes.Equal(got2, data) {
		t.Fatal("cache content differs")
	}
}

// Lifecycle: L-75
// Invariant: I-40
func TestReader_FewerThanKLive_EIO(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 2)
	ino := writeAndFlush(t, f, "a", data)
	in := f.Inode(ino)
	c, _ := f.FSM.GetChunk(in.Chunks[0])
	f.SetDown(c.Placement[0], true)
	f.SetDown(c.Placement[1], true)
	rd := f.ReaderNoCache()
	_, err := rd.ReadChunk(context.Background(), in.Chunks[0])
	if !errors.Is(err, vfs.ErrIO) {
		t.Fatalf("got %v, want vfs.ErrIO", err)
	}
}

// Lifecycle: L-68, L-105, L-166
// Invariant: I-40
func TestReader_CorruptShard_DecodesFromOthers_AndReports(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 3)
	ino := writeAndFlush(t, f, "a", data)
	in := f.Inode(ino)
	h := in.Chunks[0]
	c, _ := f.FSM.GetChunk(h)
	// corrupt the slot held by this gateway: Rank puts self first, so it is always read
	slot := -1
	for i, n := range c.Placement {
		if n == f.Target(f.SelfIdx) { // this gateway's own disk (docs/14 §9)
			slot = i
		}
	}
	if slot < 0 {
		t.Fatalf("self not in placement %v", c.Placement)
	}
	f.CorruptShard(f.SelfIdx, chunk.Key(h, c.K, c.M, slot))
	rd := f.ReaderNoCache()
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
// Invariant: I-2, I-40
func TestReader_DecodedHashMismatch_EIO(t *testing.T) {
	f := testharness.NewFabric(t, 3, 1, 1)
	// register a chunk entry whose hash does not match the bytes its shards hold
	data := testharness.Pattern(1000, 4)
	wrong := sha256.Sum256([]byte("not the data"))
	key0, key1 := chunk.Key(wrong, 1, 1, 0), chunk.Key(wrong, 1, 1, 1)
	ctx := context.Background()
	must(t, f.Put(ctx, 1, key0, data))
	must(t, f.Put(ctx, 2, key1, data))
	ino := f.Create("x")
	f.MustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{
		Ino: ino, NewSize: 1000, Writes: []meta.ChunkWrite{{Idx: 0, Hash: wrong}},
		NewChunks: []meta.ChunkDesc{{Hash: wrong, Size: 1000, K: 1, M: 1, Placement: []meta.NodeIdx{1, 2}}}, Now: 2,
	}})
	_, err := f.ReaderNoCache().ReadChunk(ctx, wrong)
	if !errors.Is(err, vfs.ErrIO) {
		t.Fatalf("got %v, want vfs.ErrIO", err)
	}
}

// Lifecycle: L-71, L-70
func TestReader_ReadAt_HolesAndShortChunks(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	cs := uint64(testharness.ChunkSize)
	// write only in chunk 2 (bytes 2cs+10..2cs+20) → chunks 0,1 are holes, chunk 2 is short
	must(t, f.Ov.Write(ino, 0, 2*cs+10, []byte("0123456789")))
	must(t, f.Flusher().FlushInode(context.Background(), ino))
	in := f.Inode(ino)
	if len(in.Chunks) != 3 || in.Chunks[0] != meta.HashZero || in.Chunks[1] != meta.HashZero || in.Chunks[2] == meta.HashZero {
		t.Fatalf("chunks %v", in.Chunks)
	}
	c, _ := f.FSM.GetChunk(in.Chunks[2])
	if c.Size != 20 || c.K != 1 { // 20-byte chunk → small-chunk policy k=1
		t.Fatalf("chunk entry %+v", c)
	}
	// truncate up through the FSM: the short chunk is zero-filled on read
	f.MustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, BaseVersion: in.Version, NewSize: 3 * cs, Now: 3}})
	in = f.Inode(ino)
	want := make([]byte, 3*cs)
	copy(want[2*cs+10:], "0123456789")
	got := make([]byte, 3*cs)
	rd := f.Reader()
	n, err := rd.ReadAt(context.Background(), in, got, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("n=%d differs at %d", n, testharness.FirstDiff(got, want))
	}
	// reads past EOF and short reads
	if n, err := rd.ReadAt(context.Background(), in, got, int64(3*cs)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF: %d %v", n, err)
	}
	if n, err := rd.ReadAt(context.Background(), in, got[:100], int64(3*cs-50)); n != 50 || !errors.Is(err, io.EOF) {
		t.Fatalf("read across EOF: %d %v", n, err)
	}
}
