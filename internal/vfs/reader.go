package vfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
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

	mu         sync.Mutex
	inflight   map[meta.Hash]*fetchCall  // one fetch per hash at a time; readahead shares it
	last       map[meta.Ino]uint64       // the chunk index of each inode's previous READ (readahead)
	prefetched map[meta.Hash]struct{}    // chunks a prefetch cached that no READ has consumed yet
	pf         map[meta.Ino][]*fetchCall // the prefetches each inode's sequential READs started (I-203)
	lat        latencyRing               // recent shard GET times: the hedge delay is their p95

	slots       chan struct{} // ReadAhead prefetch slots, gateway-wide (I-166)
	prefetching atomic.Int32
	raFetched   atomic.Uint64 // chunks a prefetch fetched
	raJoined    atomic.Uint64 // READs served by a chunk a prefetch fetched or was fetching
	raCancelled atomic.Uint64 // prefetches cut because their client seeked away (I-203)
	readsEarly  atomic.Uint64 // ranges served from verified data shards rather than the decoded chunk (I-202)
	fetching    atomic.Int32  // fetches running on their own goroutine (ReadAtEarly)
}

// ReadAhead is the most prefetches in flight gateway-wide, and the most chunks a
// sequential READ prefetches beyond the one it asks for (docs/04 §5, I-166).
const ReadAhead = 4

// ReadAheadBytes is how far ahead of a sequential client readahead reaches: the
// per-stream depth is ReadAheadBytes in chunks, at least one and at most ReadAhead, so a
// 4 MiB volume prefetches two chunks where a 1 MiB volume prefetches four (I-203).
const ReadAheadBytes = 8 << 20

// ReadaheadDepth is the chunks a sequential READ prefetches at the given chunk size.
func ReadaheadDepth(chunkSize uint32) int {
	if chunkSize == 0 {
		return ReadAhead
	}
	return max(1, min(ReadAhead, ReadAheadBytes/int(chunkSize)))
}

// hintCap bounds the per-inode and per-hash bookkeeping of readahead: past it the maps
// are cleared, costing at worst one missed prefetch per inode.
const hintCap = 4096

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

// p95 is the 95th percentile of the recorded durations; ok is false before any was.
func (r *latencyRing) p95() (d time.Duration, ok bool) {
	r.mu.Lock()
	n := r.n
	s := append([]time.Duration(nil), r.d[:n]...)
	r.mu.Unlock()
	if n == 0 {
		return 0, false
	}
	slices.Sort(s)
	return s[(n*95+99)/100-1], true
}

// hedgeDelay is the p95 of recent GETs clamped to [hedgeMin, hedgeMax], or hedgeDefault
// before any GET completed.
func (r *latencyRing) hedgeDelay() time.Duration {
	p, ok := r.p95()
	if !ok {
		return hedgeDefault
	}
	return min(max(p, hedgeMin), hedgeMax)
}

// fetchCall is a fetch in progress that later readers of the same chunk wait on.
type fetchCall struct {
	done     chan struct{}
	data     []byte
	err      error
	prefetch bool               // started by readahead, not by a READ
	joined   bool               // a READ waited on this prefetch (under Reader.mu; counted once, not again on the cache hit)
	cancel   context.CancelFunc // a prefetch's context: cut when its client seeks away unjoined (I-203)

	// A data shard that arrived and hashed to the chunk's recorded sum is published
	// here, so a reader waiting for a range inside it copies its bytes before the chunk
	// is decoded (docs/04 §5, I-202). ready[j] closes when shards[j] is set, and at
	// completion for every shard that never came.
	mu       sync.Mutex
	shards   [][]byte
	ready    []chan struct{}
	finished bool
}

// arm sizes the per-slot tables for k data shards; idempotent. The caller holds mu.
func (c *fetchCall) arm(k int) {
	if c.ready != nil {
		return
	}
	c.ready = make([]chan struct{}, k)
	for j := range c.ready {
		c.ready[j] = make(chan struct{})
	}
	c.shards = make([][]byte, k)
}

// closedSlot is what slot returns once the fetch is over: a reader that arrives after
// finish must not arm channels nobody will close.
var closedSlot = func() chan struct{} { ch := make(chan struct{}); close(ch); return ch }()

// slot returns the channel that closes once data shard j is published or the fetch is over.
func (c *fetchCall) slot(k, j int) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return closedSlot
	}
	c.arm(k)
	return c.ready[j]
}

// publish hands a verified data shard to the readers waiting for it.
func (c *fetchCall) publish(k, j int, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	c.arm(k)
	if c.shards[j] == nil {
		c.shards[j] = b
		close(c.ready[j])
	}
}

// shard returns published data shard j, or nil.
func (c *fetchCall) shard(j int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j < len(c.shards) {
		return c.shards[j]
	}
	return nil
}

// finish wakes every reader still waiting for a shard: the fetch is over, done decides.
func (c *fetchCall) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finished = true
	for j := range c.ready {
		if c.shards[j] == nil {
			close(c.ready[j])
		}
	}
}

// NewReader wires a reader. cache may be nil (then nothing is prefetched: a prefetch has
// nowhere to leave its result).
func NewReader(st *meta.Store, peers meta.Peers, shards chunk.ShardClient, cache *chunk.Cache, codecs *ec.Pool) *Reader {
	if codecs == nil {
		codecs = &ec.Pool{}
	}
	return &Reader{
		st: st, peers: peers, shards: shards, cache: cache, codecs: codecs, log: slog.Default(),
		inflight: map[meta.Hash]*fetchCall{}, last: map[meta.Ino]uint64{}, prefetched: map[meta.Hash]struct{}{},
		pf:    map[meta.Ino][]*fetchCall{},
		slots: make(chan struct{}, ReadAhead),
	}
}

// Prefetching reports the prefetches in flight; it never exceeds ReadAhead (I-166).
func (r *Reader) Prefetching() int { return int(r.prefetching.Load()) }

// ReadaheadStats reports the chunks readahead fetched and the READs it served (docs/12 §8).
func (r *Reader) ReadaheadStats() (fetched, joined uint64) {
	return r.raFetched.Load(), r.raJoined.Load()
}

// ReadaheadCancelled reports the prefetches cut because their client seeked away (I-203).
func (r *Reader) ReadaheadCancelled() uint64 { return r.raCancelled.Load() }

// ReadsEarly reports the ranges served from verified data shards rather than from the
// decoded chunk (docs/12 §8, I-202).
func (r *Reader) ReadsEarly() uint64 { return r.readsEarly.Load() }

// Fetching reports the fetches ReadAtEarly left running on their own goroutine (tests
// wait for them before the cache's directory goes).
func (r *Reader) Fetching() int { return int(r.fetching.Load()) }

// Prefetch is called by File.ReadAt once its read at off is served: a sequential READ
// (chunk 0, or the chunk after the inode's previous READ) starts a background fetch of
// the next ReadaheadDepth chunks into the cache (docs/04 §5). A hole, a cached chunk and
// one already in flight are skipped; so is every candidate once the ReadAhead slots are
// busy, rather than queued. A prefetch is registered in the in-flight table before it
// starts, so a READ that reaches its chunk joins it; its result goes only to the cache
// and its failure is logged at debug (I-166). Its shard GETs run as background work on
// the holders, and a READ of the inode that leaves the sequence cancels the inode's
// prefetches no READ has joined (I-203).
func (r *Reader) Prefetch(in meta.Inode, off int64) {
	if r.cache == nil || off < 0 {
		return
	}
	cs := r.st.Volume().ChunkSize
	idx := uint64(off) / uint64(cs)
	r.mu.Lock()
	prev, seen := r.last[in.Ino]
	if len(r.last) >= hintCap {
		clear(r.last)
		clear(r.pf)
	}
	r.last[in.Ino] = idx
	if seen && idx == prev { // another READ in the same chunk: still in the sequence, its readahead already started
		r.mu.Unlock()
		return
	}
	sequential := idx == 0 || (seen && idx == prev+1)
	if !sequential {
		r.cancelUnjoinedLocked(in.Ino)
	}
	r.mu.Unlock()
	if !sequential {
		return
	}
	depth := uint64(ReadaheadDepth(cs))
	for j := idx + 1; j <= idx+depth && j < uint64(len(in.Chunks)); j++ {
		h := in.Chunks[j]
		if h == meta.HashZero || r.cache.Has(h) {
			continue
		}
		select {
		case r.slots <- struct{}{}:
		default:
			return // every slot busy: skipped, never queued
		}
		r.mu.Lock()
		if _, running := r.inflight[h]; running {
			r.mu.Unlock()
			<-r.slots
			continue
		}
		pctx, cancel := context.WithCancel(chunk.Background(context.Background()))
		call := &fetchCall{done: make(chan struct{}), prefetch: true, cancel: cancel}
		r.inflight[h] = call
		r.pf[in.Ino] = append(r.pruneLocked(r.pf[in.Ino]), call)
		r.mu.Unlock()
		r.prefetching.Add(1)
		go func() {
			defer func() {
				r.prefetching.Add(-1)
				<-r.slots
				cancel()
			}()
			if _, err := r.run(pctx, h, call); err != nil {
				if !errors.Is(err, context.Canceled) {
					r.log.Debug("readahead fetch failed", "hash", fmt.Sprintf("%x", h[:8]), "err", err)
				}
				return
			}
			r.raFetched.Add(1)
		}()
	}
}

// cancelUnjoinedLocked cuts the inode's prefetches still in flight that no READ has
// joined: the client moved elsewhere, so their bytes would only take a holder's slot
// and the link from the chunk it wants now. The caller holds r.mu.
func (r *Reader) cancelUnjoinedLocked(ino meta.Ino) {
	for _, call := range r.pf[ino] {
		select {
		case <-call.done:
			continue
		default:
		}
		if !call.joined {
			call.cancel()
			r.raCancelled.Add(1)
		}
	}
	delete(r.pf, ino)
}

// pruneLocked drops the finished calls from an inode's prefetch list.
func (r *Reader) pruneLocked(calls []*fetchCall) []*fetchCall {
	live := calls[:0]
	for _, call := range calls {
		select {
		case <-call.done:
		default:
			live = append(live, call)
		}
	}
	return live
}

// run performs the fetch registered as call and settles every waiter.
func (r *Reader) run(ctx context.Context, h meta.Hash, call *fetchCall) ([]byte, error) {
	call.data, call.err = r.fetchChunk(ctx, h, call)
	call.finish()
	r.mu.Lock()
	delete(r.inflight, h)
	if call.prefetch && call.err == nil && !call.joined {
		if len(r.prefetched) >= hintCap {
			clear(r.prefetched)
		}
		r.prefetched[h] = struct{}{}
	}
	r.mu.Unlock()
	close(call.done)
	return call.data, call.err
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
			r.mu.Lock()
			if _, byPrefetch := r.prefetched[h]; byPrefetch {
				delete(r.prefetched, h)
				r.raJoined.Add(1)
			}
			r.mu.Unlock()
			return b, nil
		}
	}
	for {
		r.mu.Lock()
		call, running := r.inflight[h]
		if !running {
			call = &fetchCall{done: make(chan struct{})}
			r.inflight[h] = call
		} else if call.prefetch && !call.joined { // under r.mu, so a seek's cancel never cuts a joined call
			call.joined = true
			r.raJoined.Add(1)
		}
		r.mu.Unlock()
		if !running {
			return r.run(ctx, h, call)
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

// fetchChunk is the uncached read: shards, decode, verify, cache. call, when set, is
// handed each data shard that verified against its recorded sum (I-202).
func (r *Reader) fetchChunk(ctx context.Context, h meta.Hash, call *fetchCall) ([]byte, error) {
	c, ok := r.st.GetChunk(h)
	if !ok {
		return nil, fmt.Errorf("%w: chunk %x not in metadata", ErrIO, h[:8])
	}
	data, err := r.fetch(ctx, c, call)
	if err != nil {
		return nil, err
	}
	if sha256Sum(data) != h {
		r.log.Error("decoded chunk hash mismatch", "hash", fmt.Sprintf("%x", h[:8]))
		r.Events.Record(h, meta.EvReadBadChunk, "k shards decoded to the wrong hash") // E-134
		return nil, fmt.Errorf("%w: chunk %x decoded to wrong hash", ErrIO, h[:8])
	}
	if r.cache != nil {
		r.cache.Put(h, data)
	}
	return data, nil
}

// fetch gathers k good shards in rank order, pulling further slots on failure, and decodes.
// A data shard of a chunk with recorded sums is checked against its sum as it arrives: a
// mismatch is a bad slot like a corrupt frame, a match is published to call (I-202).
func (r *Reader) fetch(ctx context.Context, c meta.Chunk, call *fetchCall) ([]byte, error) {
	k, total := int(c.K), int(c.K)+int(c.M)
	sums := len(c.Sums) == k
	codec, err := r.codecs.Get(c.K, c.M)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIO, err)
	}
	// Live holders only (L-131): a dead node would cost the dial or header timeout for
	// nothing. The ranked order is then stably partitioned: this host's own shards first
	// (no network), then data slots, then parity, whatever the RTT, so a decode is only
	// needed when a data holder is dead or hedged (L-63).
	live := meta.LiveSet(r.peers.Live())
	slotOf := map[meta.NodeIdx]int{}
	var holders []meta.NodeIdx
	for i, n := range c.Placement {
		if n != 0 && live[n] {
			slotOf[n] = i
			holders = append(holders, n)
		}
	}
	local := r.localTargets()
	order := make([]int, 0, len(holders))
	for _, n := range r.peers.Rank(holders) {
		order = append(order, slotOf[n])
	}
	tier := func(slot int) int {
		switch {
		case local[c.Placement[slot]]:
			return 0
		case slot < k:
			return 1
		default:
			return 2
		}
	}
	slices.SortStableFunc(order, func(a, b int) int { return tier(a) - tier(b) })
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
	out := make(chan res, len(order)) // every launched GET can deliver without a reader
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // the GETs still in flight once k shards are in (or on failure) stop here
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
			switch {
			case x.err != nil:
				if errors.Is(x.err, chunk.ErrNotFound) || errors.Is(x.err, chunk.ErrCorrupt) {
					r.Events.Record(c.Hash, meta.EvReadBadSlot, fmt.Sprintf("slot %d node %d: %v", x.slot, c.Placement[x.slot], x.err))
					if r.BadSlot != nil {
						r.BadSlot(chunk.Key(c.Hash, c.K, c.M, x.slot), c.Placement[x.slot])
					}
				}
			case sums && x.slot < k && sha256Sum(x.b) != c.Sums[x.slot]:
				r.Events.Record(c.Hash, meta.EvReadBadSlot, fmt.Sprintf("slot %d node %d: shard does not match its recorded sum", x.slot, c.Placement[x.slot]))
				if r.BadSlot != nil {
					r.BadSlot(chunk.Key(c.Hash, c.K, c.M, x.slot), c.Placement[x.slot])
				}
			default:
				shards[x.slot] = x.b
				good++
				r.lat.add(x.took)
				if sums && x.slot < k && call != nil {
					call.publish(k, x.slot, x.b)
				}
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

// localTargets is this host and its own disks: shards there cost no network.
func (r *Reader) localTargets() map[meta.NodeIdx]bool {
	self := r.peers.Self()
	local := map[meta.NodeIdx]bool{self: true}
	nodes := r.st.Nodes()
	for i := range nodes {
		if nodes[i].Kind == meta.KindDisk && nodes[i].Host == self {
			local[nodes[i].Idx] = true
		}
	}
	return local
}

// BaseReader is ReadAt as the overlay wants its base callback: a read past the committed
// size is not an error (the overlay zero-fills what the base does not cover). The flusher
// rebuilds chunks from it, so it never returns early (I-202).
func (r *Reader) BaseReader(ctx context.Context, in meta.Inode) func(off uint64, p []byte) error {
	return func(off uint64, p []byte) error {
		_, err := r.ReadAt(ctx, in, p, int64(off))
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

// BaseReaderEarly is BaseReader over ReadAtEarly, for a client's read of a dirty file.
func (r *Reader) BaseReaderEarly(ctx context.Context, in meta.Inode) func(off uint64, p []byte) error {
	return func(off uint64, p []byte) error {
		_, err := r.ReadAtEarly(ctx, in, p, int64(off))
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

// ReadAtEarly is ReadAt for a client: a range inside one data shard of a chunk whose sums
// are recorded is returned as soon as that shard arrived and matched its sum, before the
// chunk is decoded and hashed (docs/04 §5, I-202). Everything else (a range across two
// shards, a chunk without sums, a shard whose holder is dead or whose GET failed) waits
// for the whole chunk as ReadAt does. The flusher, repair, scrub and the pins worker use
// ReadAt and ReadChunk, never this.
func (r *Reader) ReadAtEarly(ctx context.Context, in meta.Inode, p []byte, off int64) (int, error) {
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
		if err := r.readRange(ctx, h, inChunk, dst); err != nil {
			return 0, err
		}
		pos += want
	}
	if uint64(len(p)) > n {
		return int(n), io.EOF
	}
	return int(n), nil
}

// readRange fills dst with chunk h's bytes from inChunk on (zero beyond the chunk's size),
// from one verified data shard when the range lies within it and the fetch is still under
// way, else from the whole chunk.
func (r *Reader) readRange(ctx context.Context, h meta.Hash, inChunk uint64, dst []byte) error {
	clear(dst)
	if h == meta.HashZero {
		return nil
	}
	fill := func(data []byte) {
		if uint64(len(data)) > inChunk {
			copy(dst, data[inChunk:])
		}
	}
	if r.cache != nil {
		if b, ok := r.cache.Get(h); ok {
			r.mu.Lock()
			if _, byPrefetch := r.prefetched[h]; byPrefetch {
				delete(r.prefetched, h)
				r.raJoined.Add(1)
			}
			r.mu.Unlock()
			fill(b)
			return nil
		}
	}
	c, ok := r.st.GetChunk(h)
	if !ok {
		return fmt.Errorf("%w: chunk %x not in metadata", ErrIO, h[:8])
	}
	k := int(c.K)
	lo, hi := inChunk, min(inChunk+uint64(len(dst)), uint64(c.Size))
	call := r.joinOrStart(ctx, h)
	if len(c.Sums) == k && lo < hi {
		s := uint64(ec.ShardLen(int(c.Size), k)) // data shard j holds chunk bytes [j·s, (j+1)·s)
		j0, j1 := int(lo/s), int((hi-1)/s)
		have := true
		for j := j0; j <= j1 && have; j++ {
			select {
			case <-call.slot(k, j):
			case <-call.done: // settled between the fetch's finish and the reader's arrival
			case <-ctx.Done():
				return ctx.Err()
			}
			have = call.shard(j) != nil
		}
		if have { // served from the verified shards, whether or not the decode has caught up
			for j := j0; j <= j1; j++ {
				b, start := call.shard(j), uint64(j)*s
				from, to := max(lo, start), min(hi, start+s)
				copy(dst[from-lo:to-lo], b[from-start:to-start])
			}
			r.readsEarly.Add(1)
			return nil
		}
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if call.err != nil {
		if errors.Is(call.err, context.Canceled) { // a prefetch cut under us: fetch for ourselves
			data, err := r.ReadChunk(ctx, h)
			if err != nil {
				return err
			}
			fill(data)
			return nil
		}
		return call.err
	}
	fill(call.data)
	return nil
}

// joinOrStart returns the in-flight fetch of h, joining a prefetch, or registers one and
// runs it on its own goroutine so the caller can wait for a single shard of it. The fetch
// outlives the caller's context: its result is the cache's.
func (r *Reader) joinOrStart(ctx context.Context, h meta.Hash) *fetchCall {
	r.mu.Lock()
	call, running := r.inflight[h]
	if !running {
		call = &fetchCall{done: make(chan struct{})}
		r.inflight[h] = call
	} else if call.prefetch && !call.joined {
		call.joined = true
		r.raJoined.Add(1)
	}
	r.mu.Unlock()
	if !running {
		r.fetching.Add(1)
		go func() {
			defer r.fetching.Add(-1)
			_, _ = r.run(context.WithoutCancel(ctx), h, call)
		}()
	}
	return call
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
