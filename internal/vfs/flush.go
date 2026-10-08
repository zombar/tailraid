package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
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
	Workers        int
	RetryMin       time.Duration
	RetryMax       time.Duration
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		IdleFlush: 2 * time.Second, FileDirtyHi: 32 << 20, TotalDirtyHi: 256 << 20,
		PutDeadline: 10 * time.Second, ProposeTimeout: 5 * time.Second, ShutdownFlush: 30 * time.Second,
		Workers: 4, RetryMin: time.Second, RetryMax: 60 * time.Second,
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
	cache  *chunk.Cache
	clk    clock.Clock
	cfg    Config
	log    *slog.Logger
	codecs *ec.Pool
	Hooks  Hooks
	Events *meta.EventRing // chunk history (nil unless dev.enabled)

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
	rd *Reader, cache *chunk.Cache, clk clock.Clock, cfg Config, log *slog.Logger) *Flusher {
	if log == nil {
		log = slog.Default()
	}
	return &Flusher{ov: ov, st: st, prop: prop, peers: peers, shards: shards, rd: rd, cache: cache, clk: clk, cfg: cfg,
		log: log, codecs: &ec.Pool{}, backoff: map[meta.Ino]backoffState{}, full: map[meta.NodeIdx]time.Time{}}
}

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
		total := f.ov.TotalDirty()
		dirty := f.ov.Dirty()
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
		return err
	}
	return f.ov.CommitFlushed(plan, newSize)
}

// chunkWork is one touched chunk of a plan.
type chunkWork struct {
	idx      uint64
	data     []byte // nil for a hole
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
		works, err := f.assemble(ctx, plan, base, partial)
		if err != nil {
			return 0, err
		}
		if err := f.resolveAndUpload(ctx, works, forceUpload); err != nil {
			return 0, err
		}
		if f.Hooks.AfterAllShardsAcked != nil {
			f.Hooks.AfterAllShardsAcked()
		}
		cmd := f.buildCommit(plan, base, works, partial)
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
				if w.data != nil {
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
				if w.data != nil && !w.uploaded {
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

// assemble computes the touched chunk set (step 3) and their bytes (steps 4–5).
func (f *Flusher) assemble(ctx context.Context, plan *FlushPlan, base meta.Inode, partial bool) ([]*chunkWork, error) {
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
	idxs := make([]uint64, 0, len(touched))
	for i := range touched {
		if i < n {
			idxs = append(idxs, i)
		}
	}
	sort.Slice(idxs, func(a, b int) bool { return idxs[a] < idxs[b] })
	works := make([]*chunkWork, len(idxs))
	baseRead := func(off uint64, p []byte) error {
		_, err := f.rd.ReadAt(ctx, base, p, int64(off))
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	// chunks are assembled (base fetch, WAL read, hash) in parallel; works keeps idx order
	err := parallel(ctx, f.cfg.Workers, len(idxs), func(j int) error {
		i := idxs[j]
		buf := make([]byte, min(cs, plan.Size-i*cs))
		if err := f.ov.ReadPlanRange(plan, i*cs, buf, baseRead); err != nil {
			return fmt.Errorf("assemble chunk %d: %w", i, err)
		}
		w := &chunkWork{idx: i}
		if !allZero(buf) {
			w.data = buf
			w.hash = sha256Sum(buf)
		}
		works[j] = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return works, nil
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

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// resolveAndUpload runs steps 6–9 for every non-hole chunk, chunks in parallel.
func (f *Flusher) resolveAndUpload(ctx context.Context, works []*chunkWork, forceUpload bool) error {
	vol := f.st.Volume()
	return parallel(ctx, f.cfg.Workers, len(works), func(i int) error {
		w := works[i]
		if w.data == nil {
			return nil
		}
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
				return f.fillSlots(ctx, w, c, keep)
			}
		}
		w.k, w.m = vol.K, vol.M
		if meta.IsSmallChunk(uint32(len(w.data)), vol.ChunkSize) {
			w.k = 1 // replication for small chunks (docs/03)
		}
		return f.upload(ctx, w, nil)
	})
}

// presentSlots returns c's placement with every slot that is not both on a live node and
// actually holding the shard zeroed, and the count of the slots that are. Presence is
// checked with HEAD rather than assumed from liveness: a live node may have lost the
// file (L-183), and a dedup'd Commit must never point at an unreadable chunk (L-176).
func (f *Flusher) presentSlots(ctx context.Context, c meta.Chunk) ([]meta.NodeIdx, int) {
	live := map[meta.NodeIdx]bool{}
	for _, n := range f.peers.Live() {
		live[n] = true
	}
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
	live := map[meta.NodeIdx]bool{}
	for _, n := range f.peers.Live() {
		live[n] = true
	}
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

// upload encodes w and PUTs every slot (step 9). keep, when non-nil, fixes slots that
// already hold acked shards (fill path); only nil slots are placed.
func (f *Flusher) upload(ctx context.Context, w *chunkWork, keep []meta.NodeIdx) error {
	total := int(w.k) + int(w.m)
	codec, err := f.codecs.Get(w.k, w.m)
	if err != nil {
		return err
	}
	shards, err := codec.Encode(w.data)
	if err != nil {
		return err
	}
	place := make([]meta.NodeIdx, total)
	copy(place, keep)
	var held []meta.NodeIdx
	for _, n := range place {
		if n != 0 {
			held = append(held, n)
		}
	}
	fresh := meta.Place(w.hash, f.candidates(held), total-len(held))
	fi := 0
	for i := range place {
		if place[i] == 0 && fi < len(fresh) {
			place[i] = fresh[fi]
			fi++
		}
	}
	acked := make([]bool, total)
	for i, n := range keep {
		if i < total && n != 0 {
			acked[i] = true
		}
	}
	failed := f.putSlots(ctx, w, shards, place, acked)
	if len(failed) > 0 {
		// re-place failed slots once, excluding failed nodes and current holders
		var exclude []meta.NodeIdx
		for i, n := range place {
			if n != 0 && (acked[i] || !failed[i]) {
				exclude = append(exclude, n)
			}
		}
		for i := range failed {
			if failed[i] {
				exclude = append(exclude, place[i])
			}
		}
		var missing []int
		for i := range place {
			if !acked[i] {
				missing = append(missing, i)
			}
		}
		alt := meta.Place(w.hash, f.candidates(exclude), len(missing))
		for j, i := range missing {
			if j < len(alt) {
				place[i] = alt[j]
			} else {
				place[i] = 0
			}
		}
		f.putSlots(ctx, w, shards, place, acked)
	}
	acks := 0
	for i := range place {
		if acked[i] {
			acks++
		} else {
			place[i] = 0
		}
	}
	if f.cache != nil {
		f.cache.Put(w.hash, w.data)
	}
	if acks < int(w.k) {
		return fmt.Errorf("%w: chunk %x: %d < %d", ErrTooFewAcks, w.hash[:8], acks, w.k)
	}
	w.place = place
	w.uploaded = true
	f.Events.Record(w.hash, meta.EvUploaded, fmt.Sprintf("%d+%d acks %d placement %v", w.k, w.m, acks, place))
	if acks < total {
		f.Events.Record(w.hash, meta.EvDegraded, fmt.Sprintf("%d of %d slots", acks, total))
	}
	return nil
}

// putSlots PUTs every unacked slot with a non-zero node in parallel and waits until ≥k
// acks and (all done or the deadline). It marks acked slots and returns the failed ones.
func (f *Flusher) putSlots(ctx context.Context, w *chunkWork, shards [][]byte, place []meta.NodeIdx, acked []bool) map[int]bool {
	type res struct {
		slot int
		err  error
	}
	out := make(chan res, len(place))
	pending := 0
	for i, n := range place {
		if n == 0 || acked[i] {
			continue
		}
		pending++
		go func(slot int, node meta.NodeIdx) {
			err := f.shards.Put(ctx, node, chunk.Key(w.hash, w.k, w.m, slot), shards[slot])
			out <- res{slot, err}
		}(i, n)
	}
	failed := map[int]bool{}
	acks := 0
	for i := range acked {
		if acked[i] {
			acks++
		}
	}
	deadline := f.clk.After(f.cfg.PutDeadline)
	for pending > 0 {
		var r res
		select {
		case r = <-out:
		case <-deadline:
			if acks >= int(w.k) {
				return failed // stragglers are recorded as missing slots
			}
			r = <-out
		}
		pending--
		if r.err != nil {
			failed[r.slot] = true
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
func (f *Flusher) fillSlots(ctx context.Context, w *chunkWork, c meta.Chunk, keep []meta.NodeIdx) error {
	w.k, w.m = c.K, c.M
	if err := f.upload(ctx, w, keep); err != nil {
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

// buildCommit assembles the Commit command (step 10) including the NewSize rules.
func (f *Flusher) buildCommit(plan *FlushPlan, base meta.Inode, works []*chunkWork, partial bool) *meta.Commit {
	cs := uint64(f.st.Volume().ChunkSize)
	cmd := &meta.Commit{Ino: plan.Ino, BaseVersion: base.Version, Now: f.clk.Now().UnixNano()}
	seen := map[meta.Hash]bool{}
	var lastFull uint64
	for _, w := range works {
		cmd.Writes = append(cmd.Writes, meta.ChunkWrite{Idx: uint32(w.idx), Hash: w.hash})
		if w.data != nil && w.uploaded && !seen[w.hash] {
			seen[w.hash] = true
			cmd.NewChunks = append(cmd.NewChunks, meta.ChunkDesc{Hash: w.hash, Size: uint32(len(w.data)), K: w.k, M: w.m, Placement: w.place})
		}
		lastFull = max(lastFull, (w.idx+1)*cs)
	}
	switch {
	case partial:
		cmd.NewSize = max(base.Size, min(lastFull, plan.Size))
		// every dirty byte outside a committed chunk stays dirty (L-27, never L-179)
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
