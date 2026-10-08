package vfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
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
	Events  *meta.EventRing // chunk history (nil unless dev.enabled)

	mu       sync.Mutex
	inflight map[meta.Hash]*fetchCall // one fetch per hash at a time; readahead shares it
	lat      latencyRing              // recent shard GET times: the hedge delay is their p95
}

// latencyRing keeps the last 64 successful shard GET durations.
type latencyRing struct {
	mu   sync.Mutex
	d    [64]time.Duration
	n, i int
}

func (r *latencyRing) add(d time.Duration) {
	r.mu.Lock()
	r.d[r.i] = d
	r.i = (r.i + 1) % len(r.d)
	if r.n < len(r.d) {
		r.n++
	}
	r.mu.Unlock()
}

// Hedge bounds: a straggler is given at least this long and at most that long before a
// further shard is requested in its place.
const (
	hedgeMin     = 20 * time.Millisecond
	hedgeMax     = 2 * time.Second
	hedgeDefault = 200 * time.Millisecond
)

// hedgeDelay is the p95 of recent GETs clamped to [hedgeMin, hedgeMax], or hedgeDefault
// before any GET completed.
func (r *latencyRing) hedgeDelay() time.Duration {
	r.mu.Lock()
	n := r.n
	s := append([]time.Duration(nil), r.d[:n]...)
	r.mu.Unlock()
	if n == 0 {
		return hedgeDefault
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return min(max(s[(n*95+99)/100-1], hedgeMin), hedgeMax)
}

// fetchCall is a fetch in progress that later readers of the same chunk wait on.
type fetchCall struct {
	done chan struct{}
	data []byte
	err  error
}

// NewReader wires a reader. cache may be nil.
func NewReader(st *meta.Store, peers meta.Peers, shards chunk.ShardClient, cache *chunk.Cache, codecs *ec.Pool) *Reader {
	if codecs == nil {
		codecs = &ec.Pool{}
	}
	return &Reader{st: st, peers: peers, shards: shards, cache: cache, codecs: codecs, log: slog.Default(), inflight: map[meta.Hash]*fetchCall{}}
}

func sha256Sum(b []byte) meta.Hash { return sha256.Sum256(b) }

// ReadChunk returns the logical bytes of chunk h. HashZero returns nil (the caller
// zero-fills). The slice may be shared with the cache and other readers: do not modify
// it. Concurrent reads of one chunk (readahead, several clients) share one fetch.
func (r *Reader) ReadChunk(ctx context.Context, h meta.Hash) ([]byte, error) {
	if h == meta.HashZero {
		return nil, nil
	}
	if r.cache != nil {
		if b, ok := r.cache.Get(h); ok {
			return b, nil
		}
	}
	for {
		r.mu.Lock()
		call, running := r.inflight[h]
		if !running {
			call = &fetchCall{done: make(chan struct{})}
			r.inflight[h] = call
		}
		r.mu.Unlock()
		if !running {
			call.data, call.err = r.fetchChunk(ctx, h)
			r.mu.Lock()
			delete(r.inflight, h)
			r.mu.Unlock()
			close(call.done)
			return call.data, call.err
		}
		select {
		case <-call.done:
			if call.err != nil && errors.Is(call.err, context.Canceled) {
				continue // the leader gave up for its own reasons; fetch for ourselves
			}
			return call.data, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// fetchChunk is the uncached read: shards, decode, verify, cache.
func (r *Reader) fetchChunk(ctx context.Context, h meta.Hash) ([]byte, error) {
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
	// Live holders only (L-131): a dead node would cost the dial or header timeout for
	// nothing. Holders are offered to Rank in slot order, so data slots come first at
	// equal rank and a decode is only needed when a data shard is unreachable (L-63).
	live := map[meta.NodeIdx]bool{}
	for _, n := range r.peers.Live() {
		live[n] = true
	}
	slotOf := map[meta.NodeIdx]int{}
	var holders []meta.NodeIdx
	for i, n := range c.Placement {
		if n != 0 && live[n] {
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
	type res struct {
		slot int
		b    []byte
		err  error
		took time.Duration
	}
	// The first k slots are requested at once; a slot whose reply is late by the hedge
	// delay (p95 of recent GETs) has the next slot in rank order requested alongside it,
	// so one slow holder costs a shard's worth of extra traffic, not its timeout.
	out := make(chan res, len(order))
	next, pending := 0, 0
	launch := func() {
		slot := order[next]
		next++
		pending++
		go func() {
			start := time.Now()
			b, err := r.shards.Get(ctx, c.Placement[slot], chunk.Key(c.Hash, c.K, c.M, slot))
			out <- res{slot, b, err, time.Since(start)}
		}()
	}
	for good+pending < k && next < len(order) {
		launch()
	}
	hedge := time.NewTimer(r.lat.hedgeDelay())
	defer hedge.Stop()
	for good < k && pending > 0 {
		select {
		case x := <-out:
			pending--
			if x.err != nil {
				if errors.Is(x.err, chunk.ErrNotFound) || errors.Is(x.err, chunk.ErrCorrupt) {
					r.Events.Record(c.Hash, meta.EvReadBadSlot, fmt.Sprintf("slot %d node %d: %v", x.slot, c.Placement[x.slot], x.err))
					if r.BadSlot != nil {
						r.BadSlot(chunk.Key(c.Hash, c.K, c.M, x.slot), c.Placement[x.slot])
					}
				}
			} else {
				shards[x.slot] = x.b
				good++
				r.lat.add(x.took)
			}
			for good+pending < k && next < len(order) { // a failure is replaced at once
				launch()
			}
		case <-hedge.C:
			if next < len(order) {
				launch()
				hedge.Reset(r.lat.hedgeDelay())
			}
		case <-ctx.Done():
			return nil, ctx.Err()
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
