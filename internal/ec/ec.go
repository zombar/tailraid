// Package ec is the Reed-Solomon codec that turns one chunk into k data + m parity shards
// and back. One chunk is one stripe; see docs/03-erasure-coding-and-placement.md.
//
// Encoding is a pure function of the chunk bytes (required for content addressing), shards
// never alias the input, and every shard of a chunk has the same length ShardLen(size, k).
// k == 1 is replication (m+1 identical copies) and m == 0 is plain splitting; neither builds
// a matrix.
package ec

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/reedsolomon"
)

// Align is the shard length granularity in bytes. Shards are padded up to a multiple of
// Align so that lengths stay SIMD friendly and padding is bounded by Align*k per chunk.
const Align = 64

// MaxShards is the Reed-Solomon limit over GF(2^8).
const MaxShards = 256

var (
	// ErrBadParams is returned by New for k < 1, m < 0 or k+m > MaxShards.
	ErrBadParams = errors.New("ec: invalid k/m")
	// ErrTooFewShards is returned by Decode when fewer than k shards are present.
	ErrTooFewShards = errors.New("ec: too few shards to decode")
	// ErrShardLen is returned when a shard slice has the wrong count or a shard the wrong
	// length for this codec.
	ErrShardLen = errors.New("ec: bad shard length")
)

// Codec encodes and decodes stripes for one fixed (k, m).
type Codec struct {
	k, m int
	rs   reedsolomon.Encoder // nil when k == 1 or m == 0
}

// New returns a codec for k data and m parity shards.
func New(k, m int) (*Codec, error) {
	if k < 1 || m < 0 || k+m > MaxShards {
		return nil, fmt.Errorf("%w: k=%d m=%d", ErrBadParams, k, m)
	}
	c := &Codec{k: k, m: m}
	if k > 1 && m > 0 {
		rs, err := reedsolomon.New(k, m)
		if err != nil {
			return nil, fmt.Errorf("ec: reedsolomon.New(%d,%d): %w", k, m, err)
		}
		c.rs = rs
	}
	return c, nil
}

// K returns the number of data shards.
func (c *Codec) K() int { return c.k }

// M returns the number of parity shards.
func (c *Codec) M() int { return c.m }

// ShardLen is the length of every shard for a chunk of size bytes split k ways:
// ceil(size/k) rounded up to Align. A zero-size chunk still has Align-byte shards.
func ShardLen(size, k int) int {
	per := (size + k - 1) / k
	n := (per + Align - 1) / Align * Align
	if n == 0 {
		n = Align
	}
	return n
}

// Encode returns k+m freshly allocated shards of ShardLen(len(chunk), k) bytes each.
func (c *Codec) Encode(chunk []byte) ([][]byte, error) {
	sl := ShardLen(len(chunk), c.k)
	buf := make([]byte, (c.k+c.m)*sl) // one allocation, then sliced; shards never alias chunk
	shards := make([][]byte, c.k+c.m)
	for i := range shards {
		shards[i] = buf[i*sl : (i+1)*sl : (i+1)*sl]
	}
	if c.k == 1 {
		for i := range shards {
			copy(shards[i], chunk)
		}
		return shards, nil
	}
	for i := 0; i < c.k; i++ {
		lo := i * sl
		if lo >= len(chunk) {
			break
		}
		copy(shards[i], chunk[lo:min(lo+sl, len(chunk))])
	}
	if c.rs != nil {
		if err := c.rs.Encode(shards); err != nil {
			return nil, fmt.Errorf("ec: encode: %w", err)
		}
	}
	return shards, nil
}

// Decode rebuilds the first size bytes of the chunk from any k present shards. A nil entry
// marks a missing shard. The caller's shards are never modified; reconstruction, when
// needed, works on a copy.
func (c *Codec) Decode(shards [][]byte, size int) ([]byte, error) {
	if len(shards) != c.k+c.m {
		return nil, fmt.Errorf("%w: got %d shards, want %d", ErrShardLen, len(shards), c.k+c.m)
	}
	sl, present := -1, 0
	for i, s := range shards {
		if s == nil {
			continue
		}
		if sl == -1 {
			sl = len(s)
		}
		if len(s) != sl || sl == 0 || sl%Align != 0 {
			return nil, fmt.Errorf("%w: shard %d has %d bytes", ErrShardLen, i, len(s))
		}
		present++
	}
	if present < c.k {
		return nil, fmt.Errorf("%w: %d of %d present, need %d", ErrTooFewShards, present, c.k+c.m, c.k)
	}
	if size < 0 || size > c.k*sl {
		return nil, fmt.Errorf("%w: size %d exceeds capacity %d", ErrShardLen, size, c.k*sl)
	}
	if c.k == 1 {
		for _, s := range shards {
			if s != nil {
				return append([]byte(nil), s[:size]...), nil
			}
		}
	}
	data := shards[:c.k]
	if missingData(shards[:c.k]) {
		// A data shard is missing: reconstruct on a copy so the caller's slice is untouched.
		work := make([][]byte, len(shards))
		copy(work, shards)
		if err := c.rs.ReconstructData(work); err != nil {
			return nil, fmt.Errorf("ec: reconstruct: %w", err)
		}
		data = work[:c.k]
	}
	out := make([]byte, 0, size)
	for i := 0; i < c.k && len(out) < size; i++ {
		out = append(out, data[i][:min(sl, size-len(out))]...)
	}
	return out, nil
}

func missingData(data [][]byte) bool {
	for _, s := range data {
		if s == nil {
			return true
		}
	}
	return false
}

// Verify reports whether the parity shards are consistent with the data shards. All k+m
// shards must be present. With m == 0 there is no parity and Verify is trivially true.
func (c *Codec) Verify(shards [][]byte) (bool, error) {
	if len(shards) != c.k+c.m {
		return false, fmt.Errorf("%w: got %d shards, want %d", ErrShardLen, len(shards), c.k+c.m)
	}
	for i, s := range shards {
		if s == nil {
			return false, fmt.Errorf("%w: shard %d missing", ErrTooFewShards, i)
		}
		if len(s) != len(shards[0]) {
			return false, fmt.Errorf("%w: shard %d has %d bytes", ErrShardLen, i, len(s))
		}
	}
	switch {
	case c.m == 0:
		return true, nil
	case c.k == 1:
		for _, s := range shards[1:] {
			if !bytes.Equal(s, shards[0]) {
				return false, nil
			}
		}
		return true, nil
	default:
		ok, err := c.rs.Verify(shards)
		if err != nil {
			return false, fmt.Errorf("ec: verify: %w", err)
		}
		return ok, nil
	}
}

// Pool caches one Codec per (k, m), since every chunk records its own policy.
type Pool struct {
	mu sync.Mutex
	m  map[[2]uint8]*Codec
}

// Get returns the shared codec for (k, m), creating it on first use.
func (p *Pool) Get(k, m uint8) (*Codec, error) {
	key := [2]uint8{k, m}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.m[key]; ok {
		return c, nil
	}
	c, err := New(int(k), int(m))
	if err != nil {
		return nil, err
	}
	if p.m == nil {
		p.m = make(map[[2]uint8]*Codec)
	}
	p.m[key] = c
	return c, nil
}
