package vfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/gurgeh/tailraid/internal/chunk"
	"github.com/gurgeh/tailraid/internal/ec"
	"github.com/gurgeh/tailraid/internal/meta"
)

// ErrIO is returned when a chunk cannot be read correctly: fewer than k good shards, a
// decoded chunk whose hash does not match, or a chunk entry missing from metadata
// (docs/04 §5). It maps to NFS3ERR_IO at the gateway.
var ErrIO = errors.New("vfs: I/O error")

// Reader reconstructs committed chunks from shards (docs/04 §5).
type Reader struct {
	st     *meta.Store
	peers  meta.Peers
	shards chunk.ShardClient
	cache  *chunk.Cache
	codecs *ec.Pool
	log    *slog.Logger

	// BadSlot, when set, is called for every slot that returned a missing or corrupt
	// shard so the leader can queue a repair (docs/06 §4 case 5). Must not block.
	BadSlot func(key chunk.ShardKey, node meta.NodeIdx)
}

// NewReader wires a reader. cache may be nil.
func NewReader(st *meta.Store, peers meta.Peers, shards chunk.ShardClient, cache *chunk.Cache, codecs *ec.Pool) *Reader {
	if codecs == nil {
		codecs = &ec.Pool{}
	}
	return &Reader{st: st, peers: peers, shards: shards, cache: cache, codecs: codecs, log: slog.Default()}
}

func sha256Sum(b []byte) meta.Hash { return sha256.Sum256(b) }

// ReadChunk returns the logical bytes of chunk h. HashZero returns nil (the caller
// zero-fills).
func (r *Reader) ReadChunk(ctx context.Context, h meta.Hash) ([]byte, error) {
	if h == meta.HashZero {
		return nil, nil
	}
	if r.cache != nil {
		if b, ok := r.cache.Get(h); ok {
			return b, nil
		}
	}
	c, ok := r.st.GetChunk(h)
	if !ok {
		return nil, fmt.Errorf("%w: chunk %x not in metadata", ErrIO, h[:8])
	}
	data, err := r.fetch(ctx, c)
	if err != nil {
		return nil, err
	}
	if sha256Sum(data) != h {
		r.log.Error("decoded chunk hash mismatch", "hash", fmt.Sprintf("%x", h[:8]))
		return nil, fmt.Errorf("%w: chunk %x decoded to wrong hash", ErrIO, h[:8])
	}
	if r.cache != nil {
		r.cache.Put(h, data)
	}
	return data, nil
}

// fetch gathers k good shards in rank order, pulling further slots on failure, and decodes.
func (r *Reader) fetch(ctx context.Context, c meta.Chunk) ([]byte, error) {
	k, total := int(c.K), int(c.K)+int(c.M)
	codec, err := r.codecs.Get(c.K, c.M)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIO, err)
	}
	// slot order: rank the holder nodes, map back to slot indexes
	slotOf := map[meta.NodeIdx]int{}
	var holders []meta.NodeIdx
	for i, n := range c.Placement {
		if n != 0 {
			slotOf[n] = i
			holders = append(holders, n)
		}
	}
	order := make([]int, 0, len(holders))
	for _, n := range r.peers.Rank(holders) {
		order = append(order, slotOf[n])
	}
	shards := make([][]byte, total)
	good := 0
	next := 0
	type res struct {
		slot int
		b    []byte
		err  error
	}
	for good < k && next < len(order) {
		want := k - good
		batch := order[next:min(next+want, len(order))]
		next += len(batch)
		out := make(chan res, len(batch))
		var wg sync.WaitGroup
		for _, slot := range batch {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				node := c.Placement[slot]
				b, err := r.shards.Get(ctx, node, chunk.Key(c.Hash, c.K, c.M, slot))
				out <- res{slot, b, err}
			}(slot)
		}
		wg.Wait()
		close(out)
		for x := range out {
			if x.err != nil {
				if r.BadSlot != nil && (errors.Is(x.err, chunk.ErrNotFound) || errors.Is(x.err, chunk.ErrCorrupt)) {
					r.BadSlot(chunk.Key(c.Hash, c.K, c.M, x.slot), c.Placement[x.slot])
				}
				continue
			}
			shards[x.slot] = x.b
			good++
		}
	}
	if good < k {
		return nil, fmt.Errorf("%w: chunk %x: %d of %d shards reachable", ErrIO, c.Hash[:8], good, k)
	}
	data, err := codec.Decode(shards, int(c.Size))
	if err != nil {
		return nil, fmt.Errorf("%w: decode chunk %x: %w", ErrIO, c.Hash[:8], err)
	}
	return data, nil
}

// ReadAt reads committed file bytes: chunk by chunk, zero-filling holes and the tail of
// a short (dedup'd under truncate-up) chunk. Returns io.EOF on a short read.
func (r *Reader) ReadAt(ctx context.Context, in meta.Inode, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("vfs: negative offset")
	}
	size := in.Size
	if uint64(off) >= size {
		return 0, io.EOF
	}
	cs := uint64(r.st.Volume().ChunkSize)
	n := min(uint64(len(p)), size-uint64(off))
	buf := p[:n]
	pos := uint64(off)
	for pos < uint64(off)+n {
		idx := pos / cs
		inChunk := pos - idx*cs
		eff := min(cs, size-idx*cs)
		want := min(eff-inChunk, uint64(off)+n-pos)
		dst := buf[pos-uint64(off) : pos-uint64(off)+want]
		var h meta.Hash
		if int(idx) < len(in.Chunks) {
			h = in.Chunks[idx]
		}
		data, err := r.ReadChunk(ctx, h)
		if err != nil {
			return 0, err
		}
		clear(dst)
		if uint64(len(data)) > inChunk {
			copy(dst, data[inChunk:])
		}
		pos += want
	}
	if uint64(len(p)) > n {
		return int(n), io.EOF
	}
	return int(n), nil
}
