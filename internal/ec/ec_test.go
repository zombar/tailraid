package ec

import (
	"bytes"
	"errors"
	"github.com/zombar/tailraid/internal/testseed"
	"math/rand"
	"testing"
	"time"
)

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

var codecs = [][2]int{{1, 0}, {1, 2}, {2, 1}, {4, 2}, {8, 3}}

func sizesFor(k int) []int {
	return []int{0, 1, 63, 64, 65, k - 1, k, k + 1, 64 << 10, (1 << 20) - 1, 1 << 20}
}

func mustNew(t *testing.T, k, m int) *Codec {
	t.Helper()
	c, err := New(k, m)
	if err != nil {
		t.Fatalf("New(%d,%d): %v", k, m, err)
	}
	return c
}

// Invariant: I-10, I-30
func TestEncodeDecode_Sizes(t *testing.T) {
	r := testseed.Seed(t)
	for _, km := range codecs {
		k, m := km[0], km[1]
		c := mustNew(t, k, m)
		for _, size := range sizesFor(k) {
			if size < 0 {
				continue
			}
			in := randBytes(r, size)
			shards, err := c.Encode(in)
			if err != nil {
				t.Fatalf("(%d,%d) size %d: Encode: %v", k, m, size, err)
			}
			if len(shards) != k+m {
				t.Fatalf("(%d,%d) size %d: got %d shards, want %d", k, m, size, len(shards), k+m)
			}
			want := ShardLen(size, k)
			for i, s := range shards {
				if len(s) != want {
					t.Fatalf("(%d,%d) size %d: shard %d len %d, want %d", k, m, size, i, len(s), want)
				}
			}
			out, err := c.Decode(shards, size)
			if err != nil {
				t.Fatalf("(%d,%d) size %d: Decode: %v", k, m, size, err)
			}
			if !bytes.Equal(out, in) {
				t.Fatalf("(%d,%d) size %d: round trip mismatch (got %d bytes)", k, m, size, len(out))
			}
		}
	}
}

// subsets yields every index subset of size n drawn from [0,total).
func subsets(total, n int) [][]int {
	var out [][]int
	var rec func(start int, cur []int)
	rec = func(start int, cur []int) {
		if len(cur) == n {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for i := start; i < total; i++ {
			rec(i+1, append(cur, i))
		}
	}
	rec(0, nil)
	return out
}

// Invariant: I-10
func TestDecode_EverySubsetOfKShards(t *testing.T) {
	r := testseed.Seed(t)
	for _, km := range [][2]int{{2, 1}, {4, 2}, {3, 2}} {
		k, m := km[0], km[1]
		c := mustNew(t, k, m)
		for _, size := range []int{1, 1000, 64*k + 1, 300 << 10} {
			in := randBytes(r, size)
			full, err := c.Encode(in)
			if err != nil {
				t.Fatal(err)
			}
			for _, keep := range subsets(k+m, k) {
				sub := make([][]byte, k+m)
				for _, i := range keep {
					sub[i] = full[i]
				}
				out, err := c.Decode(sub, size)
				if err != nil {
					t.Fatalf("(%d,%d) size %d keep %v: %v", k, m, size, keep, err)
				}
				if !bytes.Equal(out, in) {
					t.Fatalf("(%d,%d) size %d keep %v: mismatch", k, m, size, keep)
				}
				// Decode must not have mutated the caller's surviving shards.
				for _, i := range keep {
					if !bytes.Equal(sub[i], full[i]) {
						t.Fatalf("(%d,%d) keep %v: Decode mutated shard %d", k, m, keep, i)
					}
				}
			}
		}
	}
}

// Invariant: I-10
func TestDecode_FewerThanK_Fails(t *testing.T) {
	r := testseed.Seed(t)
	for _, km := range codecs {
		k, m := km[0], km[1]
		c := mustNew(t, k, m)
		in := randBytes(r, 5000)
		full, err := c.Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		for present := 0; present < k; present++ {
			sub := make([][]byte, k+m)
			for i := 0; i < present; i++ {
				sub[i] = full[i]
			}
			out, err := c.Decode(sub, len(in))
			if !errors.Is(err, ErrTooFewShards) {
				t.Fatalf("(%d,%d) with %d shards: err=%v, want ErrTooFewShards", k, m, present, err)
			}
			if out != nil {
				t.Fatalf("(%d,%d) with %d shards: returned data on failure", k, m, present)
			}
		}
		// Wrong slice length is rejected too.
		if _, err := c.Decode(full[:k+m-1], len(in)); err == nil && m > 0 {
			t.Fatalf("(%d,%d): short shard slice accepted", k, m)
		}
	}
}

// Invariant: I-10
func TestTailPadding_NotLeaked(t *testing.T) {
	for _, km := range codecs {
		k, m := km[0], km[1]
		c := mustNew(t, k, m)
		for _, size := range []int{0, 1, 63, 65, 64*k - 1, 64*k + 1, 777} {
			in := bytes.Repeat([]byte{0xAB}, size)
			shards, err := c.Encode(in)
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Decode(shards, size)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != size {
				t.Fatalf("(%d,%d) size %d: decoded %d bytes", k, m, size, len(out))
			}
			if !bytes.Equal(out, in) {
				t.Fatalf("(%d,%d) size %d: content mismatch", k, m, size)
			}
			// Asking for a size larger than what was encoded must not invent bytes beyond the
			// padded capacity.
			if _, err := c.Decode(shards, k*ShardLen(size, k)+1); err == nil {
				t.Fatalf("(%d,%d) size %d: oversize Decode accepted", k, m, size)
			}
		}
	}
}

// Invariant: I-30
func TestShardSizesEqual_AndDeterministic(t *testing.T) {
	r := testseed.Seed(t)
	for _, km := range codecs {
		k, m := km[0], km[1]
		c := mustNew(t, k, m)
		in := randBytes(r, 12345)
		a, err := c.Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		b, err := c.Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		for i := range a {
			if len(a[i]) != ShardLen(len(in), k) {
				t.Fatalf("(%d,%d) shard %d len %d", k, m, i, len(a[i]))
			}
			if !bytes.Equal(a[i], b[i]) {
				t.Fatalf("(%d,%d) shard %d differs between encodes", k, m, i)
			}
		}
		// A second codec instance for the same (k,m) must agree byte for byte.
		c2 := mustNew(t, k, m)
		d, err := c2.Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		for i := range a {
			if !bytes.Equal(a[i], d[i]) {
				t.Fatalf("(%d,%d) shard %d differs between codec instances", k, m, i)
			}
		}
	}
}

// Invariant: I-30
func TestEncode_DoesNotAliasInput(t *testing.T) {
	r := testseed.Seed(t)
	for _, km := range codecs {
		k, m := km[0], km[1]
		c := mustNew(t, k, m)
		in := randBytes(r, 64*k+17)
		shards, err := c.Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		snap := make([][]byte, len(shards))
		for i := range shards {
			snap[i] = append([]byte(nil), shards[i]...)
		}
		for i := range in {
			in[i] ^= 0xFF
		}
		for i := range shards {
			if !bytes.Equal(shards[i], snap[i]) {
				t.Fatalf("(%d,%d) shard %d changed when input was mutated", k, m, i)
			}
		}
		// And mutating a shard must not change another shard (no shared backing array).
		if len(shards) > 1 {
			shards[0][0] ^= 0xFF
			if !bytes.Equal(shards[1], snap[1]) {
				t.Fatalf("(%d,%d) shards share backing memory", k, m)
			}
		}
	}
}

// Invariant: I-30
func TestVerify_EncodedIsConsistent(t *testing.T) {
	r := testseed.Seed(t)
	for _, km := range codecs {
		k, m := km[0], km[1]
		c := mustNew(t, k, m)
		for _, size := range []int{0, 1, 4096, 1 << 20} {
			shards, err := c.Encode(randBytes(r, size))
			if err != nil {
				t.Fatal(err)
			}
			ok, err := c.Verify(shards)
			if err != nil || !ok {
				t.Fatalf("(%d,%d) size %d: Verify ok=%v err=%v", k, m, size, ok, err)
			}
		}
	}
}

// Invariant: I-30
func TestVerify_DetectsCorruption(t *testing.T) {
	r := testseed.Seed(t)
	for _, km := range codecs {
		k, m := km[0], km[1]
		if m == 0 {
			continue // no parity: nothing to verify against
		}
		c := mustNew(t, k, m)
		shards, err := c.Encode(randBytes(r, 10000))
		if err != nil {
			t.Fatal(err)
		}
		for i := range shards {
			corrupt := make([][]byte, len(shards))
			for j := range shards {
				corrupt[j] = append([]byte(nil), shards[j]...)
			}
			corrupt[i][len(corrupt[i])/2] ^= 0x01
			ok, err := c.Verify(corrupt)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatalf("(%d,%d): corruption in shard %d not detected", k, m, i)
			}
		}
		// Missing shard: Verify needs all k+m.
		partial := append([][]byte(nil), shards...)
		partial[0] = nil
		if _, err := c.Verify(partial); err == nil {
			t.Fatalf("(%d,%d): Verify accepted a missing shard", k, m)
		}
	}
}

func TestNew_RejectsBadParams(t *testing.T) {
	for _, km := range [][2]int{{0, 0}, {1, -1}, {200, 100}, {0, 5}, {257, 0}} {
		if _, err := New(km[0], km[1]); !errors.Is(err, ErrBadParams) {
			t.Errorf("New(%d,%d): err=%v, want ErrBadParams", km[0], km[1], err)
		}
	}
	for _, km := range [][2]int{{1, 0}, {1, 255}, {255, 1}, {128, 128}} {
		c, err := New(km[0], km[1])
		if err != nil {
			t.Errorf("New(%d,%d): %v", km[0], km[1], err)
			continue
		}
		if c.K() != km[0] || c.M() != km[1] {
			t.Errorf("New(%d,%d): K/M = %d/%d", km[0], km[1], c.K(), c.M())
		}
	}
}

func TestShardLen(t *testing.T) {
	cases := []struct{ size, k, want int }{
		{0, 1, 64}, {0, 4, 64}, {1, 1, 64}, {64, 1, 64}, {65, 1, 128},
		{1, 2, 64}, {128, 2, 64}, {129, 2, 128}, {1 << 20, 4, 1 << 18},
		{(1 << 20) + 1, 4, (1 << 18) + 64}, {1000, 3, 384},
	}
	for _, c := range cases {
		if got := ShardLen(c.size, c.k); got != c.want {
			t.Errorf("ShardLen(%d,%d)=%d want %d", c.size, c.k, got, c.want)
		}
	}
}

func TestDecode_RejectsWrongShardLen(t *testing.T) {
	c := mustNew(t, 2, 1)
	shards, err := c.Encode(make([]byte, 1000))
	if err != nil {
		t.Fatal(err)
	}
	shards[2] = shards[2][:len(shards[2])-1]
	if _, err := c.Decode(shards, 1000); !errors.Is(err, ErrShardLen) {
		t.Fatalf("err=%v want ErrShardLen", err)
	}
}

func TestPool_ReusesCodec(t *testing.T) {
	var p Pool
	a, err := p.Get(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Get(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("Pool returned distinct codecs for the same (k,m)")
	}
	c, err := p.Get(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("Pool returned the same codec for different (k,m)")
	}
	if _, err := p.Get(0, 1); !errors.Is(err, ErrBadParams) {
		t.Fatalf("Pool.Get(0,1): err=%v", err)
	}
}

// Invariant: I-10, I-30
func TestRoundTrip_Property(t *testing.T) {
	r := testseed.Seed(t)
	deadline := time.Now().Add(2 * time.Second)
	var pool Pool
	iters := 0
	for time.Now().Before(deadline) {
		iters++
		k := 1 + r.Intn(8)
		m := r.Intn(4)
		c, err := pool.Get(uint8(k), uint8(m))
		if err != nil {
			t.Fatal(err)
		}
		size := r.Intn(2 << 20)
		in := randBytes(r, size)
		shards, err := c.Encode(in)
		if err != nil {
			t.Fatalf("iter %d (%d,%d) size %d: %v", iters, k, m, size, err)
		}
		// Drop m random shards.
		perm := r.Perm(k + m)
		sub := make([][]byte, k+m)
		for _, i := range perm[:k] {
			sub[i] = shards[i]
		}
		out, err := c.Decode(sub, size)
		if err != nil {
			t.Fatalf("iter %d (%d,%d) size %d keep %v: %v", iters, k, m, size, perm[:k], err)
		}
		if !bytes.Equal(out, in) {
			t.Fatalf("iter %d (%d,%d) size %d keep %v: mismatch", iters, k, m, size, perm[:k])
		}
	}
	t.Logf("%d iterations", iters)
}

func BenchmarkEncode_4_2_1MiB(b *testing.B) {
	c, err := New(4, 2)
	if err != nil {
		b.Fatal(err)
	}
	in := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(in) //nolint:gosec // benchmark data
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Encode(in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecode_4_2_1MiB_OneMissing(b *testing.B) {
	c, err := New(4, 2)
	if err != nil {
		b.Fatal(err)
	}
	in := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(in) //nolint:gosec // benchmark data
	full, err := c.Encode(in)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sub := append([][]byte(nil), full...)
		sub[1] = nil
		if _, err := c.Decode(sub, len(in)); err != nil {
			b.Fatal(err)
		}
	}
}
