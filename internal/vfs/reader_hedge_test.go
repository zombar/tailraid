package vfs_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// stall is a shard client on which one node never answers a GET.
type stall struct {
	chunk.ShardClient
	node    meta.NodeIdx
	release chan struct{}
}

func (s *stall) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	if node == s.node {
		<-s.release
		return nil, context.Canceled
	}
	return s.ShardClient.Get(ctx, node, key)
}

// A holder that never answers does not cost the read its timeout: after the hedge delay
// the next shard in rank order is requested and the chunk is decoded from the k that
// answered, one extra GET in all.
func TestReader_HedgesStalledHolder(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(cs, 6)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	c, _ := f.FSM.GetChunk(in.Chunks[0])
	victim := f.Rank(c.Placement)[0] // the holder the reader asks first
	s := &stall{ShardClient: f, node: victim, release: make(chan struct{})}
	defer close(s.release)
	rd := vfs.NewReader(f.FSM, f, s, nil, nil)
	before := 0
	for n := meta.NodeIdx(1); n <= 3; n++ {
		before += f.Gets(n)
	}
	got, err := rd.ReadChunk(ctx, in.Chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("differs at %d", testharness.FirstDiff(got, data))
	}
	gets := -before
	for n := meta.NodeIdx(1); n <= 3; n++ {
		gets += f.Gets(n)
	}
	if gets != 2 { // the two holders that answer; the stalled one never reached the fabric
		t.Fatalf("%d fabric GETs, want 2", gets)
	}
}
