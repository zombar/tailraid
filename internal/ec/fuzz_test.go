package ec

import (
	"bytes"
	"math/rand"
	"testing"
)

// Invariant: I-10, I-30
func FuzzRoundTrip(f *testing.F) {
	for _, km := range codecs {
		for _, size := range []int{0, 1, 63, 64, 65, 1000} {
			f.Add(uint8(km[0]), uint8(km[1]), int64(size), bytes.Repeat([]byte{byte(size)}, size))
		}
	}
	f.Add(uint8(4), uint8(2), int64(7), []byte("tailraid"))
	f.Fuzz(func(t *testing.T, k, m uint8, pick int64, data []byte) {
		if k == 0 || k > 16 || m > 8 {
			return
		}
		c, err := New(int(k), int(m))
		if err != nil {
			t.Fatalf("New(%d,%d): %v", k, m, err)
		}
		shards, err := c.Encode(data)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		want := ShardLen(len(data), int(k))
		for i, s := range shards {
			if len(s) != want {
				t.Fatalf("shard %d len %d want %d", i, len(s), want)
			}
		}
		if m > 0 {
			ok, err := c.Verify(shards)
			if err != nil || !ok {
				t.Fatalf("Verify: ok=%v err=%v", ok, err)
			}
		}
		// Keep a pseudo-random subset of exactly k shards.
		r := rand.New(rand.NewSource(pick)) //nolint:gosec // fuzz-derived
		perm := r.Perm(int(k + m))
		sub := make([][]byte, int(k+m))
		for _, i := range perm[:k] {
			sub[i] = shards[i]
		}
		out, err := c.Decode(sub, len(data))
		if err != nil {
			t.Fatalf("Decode keep %v: %v", perm[:k], err)
		}
		if !bytes.Equal(out, data) {
			t.Fatalf("round trip mismatch keep %v", perm[:k])
		}
	})
}
