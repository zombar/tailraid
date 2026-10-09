package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
)

// Config holds the flush policy (docs/04 §3, §4).
type Config struct {
	IdleFlush      time.Duration // flush an inode untouched this long
	FileDirtyHi    uint64        // partial flush of an inode's full chunks
	TotalDirtyHi   uint64        // flush the largest inodes
	PutDeadline    time.Duration // step 9: wait for stragglers at most this long
	ProposeTimeout time.Duration
	ShutdownFlush  time.Duration // FlushAll on shutdown gives up after this; the WAL keeps the rest
	Workers        int           // inode flushes under way at once
	ChunkWorkers   int           // chunks assembled, encoded and uploaded at once, flusher-wide (docs/04 §9)
	RetryMin       time.Duration
	RetryMax       time.Duration
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		IdleFlush: 2 * time.Second, FileDirtyHi: 32 << 20, TotalDirtyHi: 256 << 20,
		PutDeadline: 10 * time.Second, ProposeTimeout: 5 * time.Second, ShutdownFlush: 30 * time.Second,
		Workers: 4, ChunkWorkers: 8, RetryMin: time.Second, RetryMax: 60 * time.Second,
	}
}

// Hooks are nil-checked test seams at the flush's durability points (docs/05 §2).
// They are never set in production.
type Hooks struct {
	AfterAllShardsAcked func()
	BeforeCommit        func()
	AfterRaftCommit     func()
}

// Flusher turns dirty overlay state into committed chunks (docs/04 §4).
type Flusher struct {
	ov     *Overlay
	st     *meta.Store
	prop   meta.Proposer
	peers  meta.Peers
	shards chunk.ShardClient
	rd     *Reader
	clk    clock.Clock
	cfg    Config
	log    *slog.Logger
	codecs *ec.Pool
	Hooks  Hooks
	Events *meta.EventRing // chunk history (nil unless dev.enabled)

	sem     chan struct{} // ChunkWorkers slots: chunks in memory across every flush
	buffers atomic.Int64  // chunk and shard bytes held right now
	peak    atomic.Int64

	mu      sync.Mutex
	backoff map[meta.Ino]backoffState
	full    map[meta.NodeIdx]time.Time // nodes that answered ErrFull, excluded for a while
}

type backoffState struct {
	next  time.Time
	delay time.Duration
}

// NewFlusher wires a flusher. log may be nil.
func NewFlusher(ov *Overlay, st *meta.Store, prop meta.Proposer, peers meta.Peers, shards chunk.ShardClient,
	rd *Reader, clk clock.Clock, cfg Config, log *slog.Logger) *Flusher {
	if log == nil {
		log = slog.Default()
	}
	return &Flusher{ov: ov, st: st, prop: prop, peers: peers, shards: shards, rd: rd, clk: clk, cfg: cfg,
		log: log, codecs: &ec.Pool{}, sem: make(chan struct{}, max(1, cfg.ChunkWorkers)),
		backoff: map[meta.Ino]backoffState{}, full: map[meta.NodeIdx]time.Time{}}
}

// Buffers reports the chunk and shard bytes the flusher holds in memory right now and
// the most it has held since start: at most ChunkWorkers chunks with their shards,
// whatever the file or the plan (docs/04 §9, I-78).
func (f *Flusher) Buffers() (cur, peak uint64) {
	return uint64(max(f.buffers.Load(), 0)), uint64(max(f.peak.Load(), 0))
}

// BufferCap is the bound Buffers never exceeds at the volume's current policy:
// ChunkWorkers × (ChunkSize + (k+m) · ShardLen(ChunkSize, k)) (docs/04 §9, I-78).
func (f *Flusher) BufferCap() uint64 {
	vol := f.st.Volume()
	cs := int(vol.ChunkSize)
	k := max(int(vol.K), 1)
	return uint64(max(1, f.cfg.ChunkWorkers)) * uint64(cs+(k+int(vol.M))*ec.ShardLen(cs, k))
}

func (f *Flusher) hold(n int) {
	cur := f.buffers.Add(int64(n))
	for {
		p := f.peak.Load()
		if cur <= p || f.peak.CompareAndSwap(p, cur) {
			return
		}
	}
}

func (f *Flusher) release(n int) { f.buffers.Add(-int64(n)) }

// Errors returned by a flush that must be retried later.
var (
	ErrTooFewAcks  = errors.New("vfs: fewer than k shard acks")
	errRebaseLimit = errors.New("vfs: commit rebase limit reached")
)

// FlushInode performs a full flush of ino (steps 1–11 of docs/04 §4).
func (f *Flusher) FlushInode(ctx context.Context, ino meta.Ino) error {
	return f.flush(ctx, ino, false)
}

// FlushPartial commits only the fully dirty full-size chunks of ino (FileDirtyHi
// trigger); the partial tail stays in the overlay. Truncates are never flushed partially.
func (f *Flusher) FlushPartial(ctx context.Context, ino meta.Ino) error {
	return f.flush(ctx, ino, true)
}

// FlushAll flushes every dirty inode once (shutdown).
func (f *Flusher) FlushAll(ctx context.Context) error {
	var first error
	for _, d := range f.ov.Dirty() {
		if err := f.flush(ctx, d.Ino, false); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Run evaluates the flush triggers until ctx is done, then flushes everything once.
func (f *Flusher) Run(ctx context.Context) {
	sem := make(chan struct{}, max(1, f.cfg.Workers))
	var wg sync.WaitGroup
	timer := f.clk.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			// bounded (docs/04 §3): a dead holder must not hold the shutdown; the WAL keeps
			// whatever did not flush and the next start flushes it
			sctx, cancel := context.WithTimeout(context.Background(), f.cfg.ShutdownFlush)
			err := f.FlushAll(sctx)
			cancel()
			if err != nil {
				f.log.Warn("shutdown flush incomplete; the WAL keeps the rest", "err", err)
			}
			return
		case <-timer.C():
		}
		now := f.clk.Now()
		dirty := f.ov.Dirty()
		// the threshold weighs what this loop can still flush: bytes in outstanding plans
		// are already draining (they count toward MaxDirty, I-80, not toward the trigger)
		total := f.ov.TotalDirty()
		for _, d := range dirty {
			total -= min(total, d.PlanBytes)
		}
		sort.Slice(dirty, func(i, j int) bool { return dirty[i].DirtyBytes > dirty[j].DirtyBytes })
		for _, d := range dirty {
			if d.Flushing {
				continue
			}
			partial := false
			switch {
			case total >= f.cfg.TotalDirtyHi:
				total -= d.DirtyBytes
			case d.WALBytes >= f.ov.WALCap():
				// a rewritten region keeps dirty bytes small while the WAL grows: flush
				// the inode in full so the WAL compacts (docs/04 §3, L-186)
			case d.DirtyBytes >= f.cfg.FileDirtyHi:
				partial = true
			case now.Sub(d.LastWrite) >= f.cfg.IdleFlush:
			default:
				continue
			}
			if !f.readyToRetry(d.Ino, now) {
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				continue
			}
			wg.Add(1)
			go func(ino meta.Ino, partial bool) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := f.flush(ctx, ino, partial); err != nil {
					f.log.Warn("flush failed, will retry", "ino", ino, "err", err)
					f.scheduleRetry(ino)
				} else {
					f.clearRetry(ino)
				}
			}(d.Ino, partial)
		}
		timer.Reset(200 * time.Millisecond)
	}
}

func (f *Flusher) readyToRetry(ino meta.Ino, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.backoff[ino]
	return !ok || !now.Before(b.next)
}

func (f *Flusher) scheduleRetry(ino meta.Ino) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.backoff[ino]
	if b.delay == 0 {
		b.delay = f.cfg.RetryMin
	} else {
		b.delay = min(b.delay*2, f.cfg.RetryMax)
	}
	b.next = f.clk.Now().Add(b.delay)
	f.backoff[ino] = b
}

func (f *Flusher) clearRetry(ino meta.Ino) {
	f.mu.Lock()
	delete(f.backoff, ino)
	f.mu.Unlock()
}

// flush is FlushInode with the partial-flush variant (FileDirtyHi trigger).
func (f *Flusher) flush(ctx context.Context, ino meta.Ino, partial bool) error {
	plan, ok := f.ov.SnapshotForFlush(ino)
	if !ok {
		return nil
	}
	if plan.SizeSet {
		partial = false // truncates are never partially flushed
	}
	newSize, err := f.commitPlan(ctx, plan, partial)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			f.ov.Drop(ino)
			return nil
		}
		f.ov.AbortFlush(plan)
		f.ov.NoteFlush(ino, err) // a WRITE at the cap fails at once behind this (L-189)
		return err
	}
	f.ov.NoteFlush(ino, nil) // before CommitFlushed, which may evict the entry
	return f.ov.CommitFlushed(plan, newSize)
}

// chunkWork is what survives of a touched chunk once it is decided: the bytes themselves
// live only inside processChunk (docs/04 §9, I-78).
type chunkWork struct {
	idx      uint64
	size     uint32 // logical length
	hole     bool   // all zero: HashZero, nothing uploaded
	hash     meta.Hash
	k, m     uint8
	place    []meta.NodeIdx
	uploaded bool
}

// commitPlan runs steps 2–11 for a plan, including rebases, and returns the committed size.
func (f *Flusher) commitPlan(ctx context.Context, plan *FlushPlan, partial bool) (uint64, error) {
	forceUpload := false
	for attempt := 0; attempt < 16; attempt++ {
		base, ok := f.st.GetInode(plan.Ino)
		if !ok || base.Type != meta.TFile {
			return 0, meta.ErrNotExist
		}
		// the size this flush works against follows the base of this attempt: another
		// gateway may have grown or shrunk the file since the entry was created (L-178)
		plan.Size = plan.sizeAgainst(base.Size)
		idxs, limited := f.touched(plan, base, partial)
		// every attempt re-assembles from the WAL (step 10): holding a plan's bytes across
		// a retry would cost a plan's worth of memory per inode (I-78)
		works, err := f.process(ctx, plan, base, idxs, forceUpload)
		if err != nil {
			return 0, err
		}
		if f.Hooks.AfterAllShardsAcked != nil {
			f.Hooks.AfterAllShardsAcked()
		}
		cmd := f.buildCommit(plan, base, works, partial || limited)
		if f.Hooks.BeforeCommit != nil {
			f.Hooks.BeforeCommit()
		}
		pctx, cancel := context.WithTimeout(ctx, f.cfg.ProposeTimeout)
		res, index, perr := f.prop.Propose(pctx, meta.Command{Op: meta.OpCommit, Commit: cmd})
		cancel()
		if perr != nil {
			return 0, fmt.Errorf("propose commit: %w", perr)
		}
		switch {
		case res.Err == nil:
			if f.Hooks.AfterRaftCommit != nil {
				f.Hooks.AfterRaftCommit()
			}
			if err := f.st.WaitApplied(ctx, index); err != nil {
				return 0, err
			}
			for _, w := range works {
				if !w.hole {
					f.Events.Record(w.hash, meta.EvCommitted, fmt.Sprintf("ino %d idx %d index %d", plan.Ino, w.idx, index))
				}
			}
			return cmd.NewSize, nil
		case errors.Is(res.Err, meta.ErrConflict):
			f.log.Info("commit conflict, rebasing", "ino", plan.Ino)
			continue
		case errors.Is(res.Err, meta.ErrUnknownChunk):
			f.log.Info("dedup'd chunk vanished, re-uploading", "ino", plan.Ino)
			for _, w := range works {
				if !w.hole && !w.uploaded {
					f.Events.Record(w.hash, meta.EvUnknownChunk, "")
				}
			}
			forceUpload = true
			continue
		case errors.Is(res.Err, meta.ErrNeedTailRewrite):
			f.log.Error("commit rejected: tail rewrite missing (bug)", "ino", plan.Ino)
			return 0, res.Err
		case errors.Is(res.Err, meta.ErrNotExist), errors.Is(res.Err, meta.ErrIsDir):
			return 0, meta.ErrNotExist
		default:
			return 0, fmt.Errorf("commit rejected: %w", res.Err)
		}
	}
	return 0, errRebaseLimit
}

// touched computes the touched chunk set (step 3) in index order and cuts it to one batch
// of at most FileDirtyHi bytes of chunks (step 10, I-80); limited reports the cut. A plan
// with a truncate is never cut: its holes and tail rewrite must land in one Commit.
func (f *Flusher) touched(plan *FlushPlan, base meta.Inode, partial bool) (idxs []uint64, limited bool) {
	cs := uint64(f.st.Volume().ChunkSize)
	n := uint64(meta.ChunkCount(plan.Size, uint32(cs)))
	touched := map[uint64]bool{}
	for _, x := range plan.Extents {
		for i := x.off / cs; i*cs < x.end() && i < n; i++ {
			// a partial flush commits only full-size chunks that are entirely dirty
			if !partial || ((i+1)*cs <= plan.Size && plan.Extents.coversChunk(i, uint32(cs), plan.Size)) {
				touched[i] = true
			}
		}
	}
	if plan.SizeSet {
		if plan.MinSize%cs != 0 {
			touched[plan.MinSize/cs] = true // the chunk straddling the truncation point
		}
		for i := (plan.MinSize + cs - 1) / cs; i < n; i++ {
			if !touched[i] && (int(i) >= len(base.Chunks) || base.Chunks[i] != meta.HashZero) {
				touched[i] = true // truncated-away bytes become holes
			}
		}
	}
	idxs = make([]uint64, 0, len(touched))
	for i := range touched {
		if i < n {
			idxs = append(idxs, i)
		}
	}
	sort.Slice(idxs, func(a, b int) bool { return idxs[a] < idxs[b] })
	if batch := max(1, int(f.cfg.FileDirtyHi/cs)); !plan.SizeSet && len(idxs) > batch {
		idxs, limited = idxs[:batch], true
	}
	return idxs, limited
}

// process runs steps 4–9 for every touched chunk, chunks in parallel; works keeps idx order.
func (f *Flusher) process(ctx context.Context, plan *FlushPlan, base meta.Inode, idxs []uint64, forceUpload bool) ([]*chunkWork, error) {
	works := make([]*chunkWork, len(idxs))
	baseRead := f.rd.BaseReader(ctx, base)
	err := parallel(ctx, f.cfg.ChunkWorkers, len(idxs), func(j int) error {
		w, err := f.processChunk(ctx, plan, baseRead, idxs[j], forceUpload)
		works[j] = w
		return err
	})
	if err != nil {
		return nil, err
	}
	return works, nil
}

// processChunk assembles one chunk (steps 4–5), resolves it (6–8) and uploads it (9) as
// one pipeline under the flusher-wide semaphore. The chunk's bytes and shards exist only
// inside this call: once the chunk is decided, nothing but the work record remains (I-78).
func (f *Flusher) processChunk(ctx context.Context, plan *FlushPlan, baseRead func(off uint64, p []byte) error, i uint64, forceUpload bool) (*chunkWork, error) {
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-f.sem }()
	cs := uint64(f.st.Volume().ChunkSize)
	n := min(cs, plan.Size-i*cs)
	buf := make([]byte, n)
	f.hold(len(buf))
	defer f.release(len(buf))
	if err := f.ov.ReadPlanRange(plan, i*cs, buf, baseRead); err != nil {
		return nil, fmt.Errorf("assemble chunk %d: %w", i, err)
	}
	w := &chunkWork{idx: i, size: uint32(n)}
	if allZero(buf) {
		w.hole = true
		return w, nil
	}
	w.hash = sha256Sum(buf)
	if err := f.resolve(ctx, w, buf, forceUpload); err != nil {
		return nil, err
	}
	return w, nil
}

// parallel runs fn(0..n-1) on at most workers goroutines and returns the first error;
// the rest are cancelled through ctx.
func parallel(ctx context.Context, workers, n int, fn func(i int) error) error {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	sem := make(chan struct{}, workers)
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			i = n
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return first
}

// zeroBlock lets allZero compare whole blocks with the runtime's memequal instead of a
// byte loop.
var zeroBlock [64 << 10]byte

func allZero(b []byte) bool {
	for len(b) > 0 {
		n := min(len(b), len(zeroBlock))
		if !bytes.Equal(b[:n], zeroBlock[:n]) {
			return false
		}
		b = b[n:]
	}
	return true
}

// resolve runs steps 6–9 for one non-hole chunk whose bytes are data.
func (f *Flusher) resolve(ctx context.Context, w *chunkWork, data []byte, forceUpload bool) error {
	if !forceUpload {
		if c, ok := f.st.GetChunk(w.hash); ok {
			keep, present := f.presentSlots(ctx, c)
			if present >= int(c.K) {
				w.k, w.m, w.place = c.K, c.M, c.Placement
				f.Events.Record(w.hash, meta.EvDedupHit, fmt.Sprintf("refs %d", c.Refs))
				return nil // dedup hit, no upload (L-33, L-34)
			}
			// degraded, dead-holder or shard-less entry: fill its slots with the bytes in hand
			f.Events.Record(w.hash, meta.EvFill, fmt.Sprintf("%d present of %d", present, int(c.K)+int(c.M)))
			return f.fillSlots(ctx, w, data, c, keep)
		}
	}
	vol := f.st.Volume()
	w.k, w.m = vol.K, vol.M
	if meta.IsSmallChunk(uint32(len(data)), vol.ChunkSize) {
		w.k = 1 // replication for small chunks (docs/03)
	}
	return f.upload(ctx, w, data, nil)
}

// presentSlots returns c's placement with every slot that is not both on a live node and
// actually holding the shard zeroed, and the count of the slots that are. Presence is
// checked with HEAD rather than assumed from liveness: a live node may have lost the
// file (L-183), and a dedup'd Commit must never point at an unreadable chunk (L-176).
func (f *Flusher) presentSlots(ctx context.Context, c meta.Chunk) ([]meta.NodeIdx, int) {
	live := meta.LiveSet(f.peers.Live())
	keep := make([]meta.NodeIdx, len(c.Placement))
	var mu sync.Mutex
	var wg sync.WaitGroup
	present := 0
	for i, n := range c.Placement {
		if n == 0 || !live[n] {
			continue
		}
		wg.Add(1)
		go func(i int, n meta.NodeIdx) {
			defer wg.Done()
			ok, err := f.shards.Has(ctx, n, chunk.Key(c.Hash, c.K, c.M, i))
			if err != nil {
				f.log.Debug("shard presence check failed; treating as absent", "node", n, "slot", i, "err", err)
			}
			if err != nil || !ok {
				return
			}
			mu.Lock()
			keep[i] = n
			present++
			mu.Unlock()
		}(i, n)
	}
	wg.Wait()
	return keep, present
}

// candidates returns the live, Active, non-full nodes minus excluded ones.
func (f *Flusher) candidates(exclude []meta.NodeIdx) []meta.Candidate {
	live := meta.LiveSet(f.peers.Live())
	now := f.clk.Now()
	f.mu.Lock()
	for n, until := range f.full {
		if now.After(until) {
			delete(f.full, n)
		} else {
			delete(live, n)
		}
	}
	f.mu.Unlock()
	cands := meta.Candidates(f.st.Nodes(), func(n meta.NodeIdx) bool { return live[n] })
	return meta.ExcludeIdx(cands, exclude)
}

// upload encodes data and PUTs every slot (step 9). keep, when non-nil, fixes slots that
// already hold acked shards (fill path); only nil slots are placed. The shards exist only
// for the duration of the call (I-78).
func (f *Flusher) upload(ctx context.Context, w *chunkWork, data []byte, keep []meta.NodeIdx) error {
	total := int(w.k) + int(w.m)
	codec, err := f.codecs.Get(w.k, w.m)
	if err != nil {
		return err
	}
	shards, err := codec.Encode(data)
	if err != nil {
		return err
	}
	f.hold(total * len(shards[0]))
	defer f.release(total * len(shards[0]))
	place := make([]meta.NodeIdx, total)
	copy(place, keep)
	var held []meta.NodeIdx
	for _, n := range place {
		if n != 0 {
			held = append(held, n)
		}
	}
	meta.FillPlacement(w.hash, place, f.candidates(held), int(w.k))
	acked := make([]bool, total)
	for i, n := range keep {
		if i < total && n != 0 {
			acked[i] = true
		}
	}
	failed := f.putSlots(ctx, w, shards, place, acked)
	unacked := 0
	for i := range place {
		if place[i] != 0 && !acked[i] {
			unacked++ // failed, or still pending past the deadline (L-43)
		}
	}
	if unacked > 0 {
		// re-place every slot without an ack once, excluding failed and straggling nodes
		// and the current holders
		var exclude []meta.NodeIdx
		for i, n := range place {
			if n != 0 && (acked[i] || failed[i] == nil) {
				exclude = append(exclude, n)
			}
		}
		for i := range failed {
			exclude = append(exclude, place[i])
		}
		for i := range place {
			if !acked[i] {
				place[i] = 0
			}
		}
		meta.FillPlacement(w.hash, place, f.candidates(exclude), int(w.k))
		for i, err := range f.putSlots(ctx, w, shards, place, acked) {
			failed[i] = err
		}
	}
	acks := 0
	for i := range place {
		if acked[i] {
			acks++
		} else {
			place[i] = 0
		}
	}
	if acks < int(w.k) {
		err := fmt.Errorf("%w: chunk %x: %d < %d", ErrTooFewAcks, w.hash[:8], acks, w.k)
		if len(failed) > 0 && allFull(failed) {
			err = fmt.Errorf("%w: %w", err, chunk.ErrFull) // ENOSPC to the client, not EIO (I-79)
		}
		return err
	}
	w.place = place
	w.uploaded = true
	f.Events.Record(w.hash, meta.EvUploaded, fmt.Sprintf("%d+%d acks %d placement %v", w.k, w.m, acks, place))
	if acks < total {
		f.Events.Record(w.hash, meta.EvDegraded, fmt.Sprintf("%d of %d slots", acks, total))
	}
	return nil
}

func allFull(failed map[int]error) bool {
	for _, err := range failed {
		if !errors.Is(err, chunk.ErrFull) {
			return false
		}
	}
	return true
}

// putSlots PUTs every unacked slot with a non-zero node in parallel and waits until ≥k
// acks and (all done or the deadline). It marks acked slots and returns the failed ones
// with their errors. Stragglers past the deadline are cancelled and awaited before it
// returns, so no goroutine keeps a shard buffer alive past its chunk (L-190, I-83).
func (f *Flusher) putSlots(ctx context.Context, w *chunkWork, shards [][]byte, place []meta.NodeIdx, acked []bool) map[int]error {
	type res struct {
		slot int
		err  error
	}
	pctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	out := make(chan res, len(place))
	pending := 0
	for i, n := range place {
		if n == 0 || acked[i] {
			continue
		}
		pending++
		wg.Add(1)
		go func(slot int, node meta.NodeIdx) {
			defer wg.Done()
			err := f.shards.Put(pctx, node, chunk.Key(w.hash, w.k, w.m, slot), shards[slot])
			out <- res{slot, err}
		}(i, n)
	}
	failed := map[int]error{}
	acks := 0
	for i := range acked {
		if acked[i] {
			acks++
		}
	}
	// Once the deadline has passed, the first moment k acks are in ends the wait and the
	// stragglers are recorded as missing slots (L-43). The deadline fires once, so it is
	// remembered rather than re-selected: a deadline that passes before the k-th ack must
	// still cut the wait short when that ack arrives.
	deadline := f.clk.After(f.cfg.PutDeadline)
	expired := false
	for pending > 0 {
		if expired && acks >= int(w.k) {
			return failed
		}
		var r res
		select {
		case r = <-out:
		case <-deadline:
			expired, deadline = true, nil
			continue
		}
		pending--
		if r.err != nil {
			failed[r.slot] = r.err
			if errors.Is(r.err, chunk.ErrFull) {
				f.mu.Lock()
				f.full[place[r.slot]] = f.clk.Now().Add(time.Minute)
				f.mu.Unlock()
			}
			f.log.Warn("shard put failed", "node", place[r.slot], "slot", r.slot, "err", r.err)
			f.Events.Record(w.hash, meta.EvPutFailed, fmt.Sprintf("slot %d node %d: %v", r.slot, place[r.slot], r.err))
			continue
		}
		acked[r.slot] = true
		acks++
	}
	return failed
}

// fillSlots re-encodes with the entry's (k,m), PUTs every slot not in keep (missing,
// dead or shard-less) to fresh nodes and proposes SetPlacement, so a dedup'd commit
// never points at an unreadable chunk.
func (f *Flusher) fillSlots(ctx context.Context, w *chunkWork, data []byte, c meta.Chunk, keep []meta.NodeIdx) error {
	w.k, w.m = c.K, c.M
	if err := f.upload(ctx, w, data, keep); err != nil {
		return err
	}
	res, index, err := f.prop.Propose(ctx, meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: w.hash, K: w.k, M: w.m, Placement: w.place}})
	if err != nil {
		return fmt.Errorf("propose set-placement: %w", err)
	}
	if res.Err != nil && !errors.Is(res.Err, meta.ErrUnknownChunk) {
		return fmt.Errorf("set-placement rejected: %w", res.Err)
	}
	if res.Err == nil {
		if err := f.st.WaitApplied(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

// buildCommit assembles the Commit command (step 10) including the NewSize rules. partial
// covers both the FileDirtyHi trigger and a plan cut to one batch (I-80): either way only
// the chunks in works are committed and the rest of the plan stays dirty.
func (f *Flusher) buildCommit(plan *FlushPlan, base meta.Inode, works []*chunkWork, partial bool) *meta.Commit {
	cs := uint64(f.st.Volume().ChunkSize)
	cmd := &meta.Commit{Ino: plan.Ino, BaseVersion: base.Version, Now: f.clk.Now().UnixNano()}
	seen := map[meta.Hash]bool{}
	var lastFull uint64
	for _, w := range works {
		cmd.Writes = append(cmd.Writes, meta.ChunkWrite{Idx: uint32(w.idx), Hash: w.hash})
		if !w.hole && w.uploaded && !seen[w.hash] {
			seen[w.hash] = true
			cmd.NewChunks = append(cmd.NewChunks, meta.ChunkDesc{Hash: w.hash, Size: w.size, K: w.k, M: w.m, Placement: w.place})
		}
		lastFull = max(lastFull, (w.idx+1)*cs)
	}
	switch {
	case partial:
		cmd.NewSize = max(base.Size, min(lastFull, plan.Size))
		// every dirty byte outside a committed chunk stays dirty (L-27, L-191, never L-179)
		committed := make([]uint64, len(works))
		for i, w := range works {
			committed[i] = w.idx
		}
		plan.Residual = plan.Extents.without(committed, cs)
	case plan.SizeSet:
		cmd.HasTruncate, cmd.TruncateTo = true, plan.MinSize
		cmd.NewSize = max(plan.MinSize, plan.Extents.maxEnd())
		if plan.Size > cmd.NewSize { // truncate-up without writes
			cmd.NewSize = plan.Size
		}
	default:
		cmd.NewSize = max(base.Size, plan.Extents.maxEnd())
	}
	// Writes beyond NewSize cannot exist; drop any (defensive, partial flush)
	n := uint64(meta.ChunkCount(cmd.NewSize, uint32(cs)))
	kept := cmd.Writes[:0]
	for _, wr := range cmd.Writes {
		if uint64(wr.Idx) < n {
			kept = append(kept, wr)
		}
	}
	cmd.Writes = kept
	return cmd
}
