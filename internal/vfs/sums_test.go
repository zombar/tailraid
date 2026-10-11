package vfs_test

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// The flusher records each committed chunk's data-shard sums when every live member reads
// codec 5 and commits without them otherwise; a recorded sum is the hash of the shard
// payload the holder stores, so the read path can check a shard against it.
// Invariant: I-201
func TestFlusher_CommitCarriesSumsOnlyAtCodec5(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	for _, tc := range []struct {
		name     string
		minCodec uint8
		want     bool
	}{
		{"a member still reads codec 4", meta.CodecShardSums - 1, false},
		{"every member reads codec 5", meta.CodecShardSums, true},
		{"every member reads a newer codec", meta.CodecShardSums + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := testharness.Pattern(2*cs, byte(len(tc.name)))
			ino := f.Create(tc.name)
			must(t, f.Ov.Write(ino, 0, 0, data))
			fl := f.Flusher()
			fl.MinCodec = func() uint8 { return tc.minCodec }
			must(t, fl.FlushInode(context.Background(), ino))
			in := f.Inode(ino)
			for idx, h := range in.Chunks {
				c, ok := f.FSM.GetChunk(h)
				if !ok {
					t.Fatalf("chunk %d missing", idx)
				}
				if !tc.want {
					if c.Sums != nil {
						t.Fatalf("chunk %d carries sums %v at codec %d", idx, c.Sums, tc.minCodec)
					}
					continue
				}
				if len(c.Sums) != int(c.K) {
					t.Fatalf("chunk %d: %d sums, want %d", idx, len(c.Sums), c.K)
				}
				for j := range int(c.K) { // the sum is the hash of the shard payload the holder stores
					holder := c.Placement[j]
					payload, err := f.StoreOf(holder).Get(chunk.Key(h, c.K, c.M, j))
					must(t, err)
					if sha256.Sum256(payload) != c.Sums[j] {
						t.Fatalf("chunk %d shard %d: recorded sum differs from the stored payload's hash", idx, j)
					}
				}
			}
		})
	}
	// the default (no MinCodec hook) records sums: a single binary is always current
	data := testharness.Pattern(cs, 99)
	in := f.Inode(writeAndFlush(t, f, "plain", data))
	if c, _ := f.FSM.GetChunk(in.Chunks[0]); len(c.Sums) != int(c.K) {
		t.Fatalf("without a hook the flusher recorded %d sums", len(c.Sums))
	}
	_ = vfs.ReadAhead
}
