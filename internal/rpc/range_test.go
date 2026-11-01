package rpc

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/s3/s3test"
)

// A shard GET with a Range answers the whole block table and the bytes named, clamped to
// the payload, for every edge of the block grid; the slice of a block-aligned request
// verifies against the shard's root.
// Invariant: I-423
func TestChunkRPC_RangeGet_Table(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.MaxShard = 1 << 20 })
	c := e.client()
	const B = chunk.BlockSize
	p := bytes.Repeat([]byte{3, 1, 4, 1, 5, 9, 2, 6}, (3*B+7)/8+1)[:3*B+7]
	k := key(p, 0)
	if err := c.Put(ctx, 1, k, p); err != nil {
		t.Fatal(err)
	}
	table := chunk.BlockSums(p)
	n := int64(len(p))
	for _, off := range []int64{0, 1, B - 1, B, B + 1, 2 * B, n - 1} {
		for _, want := range []int64{1, B, B + 1, n} {
			s, err := c.GetRange(ctx, 1, k, off, want)
			if err != nil {
				t.Fatalf("range %d+%d: %v", off, want, err)
			}
			end := min(off+want, n)
			if s.Off != off || !bytes.Equal(s.Data, p[off:end]) || !reflect.DeepEqual(s.Table, table) {
				t.Fatalf("range %d+%d: off %d, %d bytes, table ok %v", off, want, s.Off, len(s.Data), reflect.DeepEqual(s.Table, table))
			}
		}
	}
	for _, r := range [][2]int64{{0, 1}, {B - 1, B + 1}, {2*B + 5, n}, {n - 1, n}} {
		a, b := chunk.BlockRange(r[0], r[1], n)
		s, err := c.GetRange(ctx, 1, k, a, b-a)
		if err != nil || !chunk.VerifySlice(s, chunk.ShardRoot(p)) {
			t.Fatalf("aligned range [%d, %d): %v, verified %v", a, b, err, err == nil && chunk.VerifySlice(s, chunk.ShardRoot(p)))
		}
	}
	if _, err := c.GetRange(ctx, 1, k, n, 1); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a start at the payload's end: %v", err)
	}
	if _, err := c.GetRange(ctx, 1, k, 0, 0); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("an empty range: %v", err)
	}
}

// A range of a missing shard is ErrNotFound, of a corrupt one ErrCorrupt (the shard is
// read whole and quarantined as for any GET), and a start past the payload is refused.
// Invariant: I-423
func TestChunkRPC_RangeGet_CorruptMissingPastEnd(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	if _, err := c.GetRange(ctx, 1, key([]byte("missing"), 0), 0, 10); !errors.Is(err, chunk.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	p := bytes.Repeat([]byte{7}, 1000)
	k := key(p, 0)
	if err := c.Put(ctx, 1, k, p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetRange(ctx, 1, k, 1000, 1); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("past the end: %v", err)
	}
	raw, err := os.ReadFile(e.store.Path(k))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(e.store.Path(k), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetRange(ctx, 1, k, 0, 10); !errors.Is(err, chunk.ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
	if e.store.Has(k) {
		t.Fatal("the corrupt shard was not quarantined")
	}
}

// An S3 node serves no slice: its object comes with no block table, so the client answers
// ErrNoRange and the reader fetches the chunk.
// Invariant: I-423
func TestClient_GetRange_S3IsNoRange(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	c := s3Client(t, srv)
	p := bytes.Repeat([]byte{5}, 4096)
	k := key(p, 2)
	if err := c.Put(ctx, 7, k, p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetRange(ctx, 7, k, 0, 1024); !errors.Is(err, chunk.ErrNoRange) {
		t.Fatalf("S3 range: %v", err)
	}
}
