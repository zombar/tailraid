package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/logx"
	"github.com/zombar/tailraid/internal/meta"
)

// Config holds the flush policy (docs/04 §3, §4).
type Config struct {
	IdleFlush      time.Duration // flush an inode untouched this long
	FileDirtyHi    uint64        // partial flush of an inode's full chunks
	TotalDirtyHi   uint64        // flush the largest inodes
	PutDeadline    time.Duration // step 9: wait for stragglers at most this long
	StragglerMin   time.Duration // step 9: the floor of the wait for stragglers once k acks are in (I-169)
	ProposeTimeout time.Duration
	ShutdownFlush  time.Duration // FlushAll on shutdown gives up after this; the WAL keeps the rest
	Workers        int           // inode flushes under way at once
	ChunkWorkers   int           // chunks assembled, encoded and uploaded at once, flusher-wide (docs/04 §9)
	RetryMin       time.Duration
	RetryMax       time.Duration
	// ReuseFor is how long after its PUT a chunk's shards are reused by a retry of the
	// flush that placed them: half the orphan sweep's grace, so a reused shard is never
	// near the age at which the sweep may take it (I-312, L-58).
	ReuseFor time.Duration
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		IdleFlush: 2 * time.Second, FileDirtyHi: 32 << 20, TotalDirtyHi: 256 << 20,
		PutDeadline: 10 * time.Second, StragglerMin: 2 * time.Second, ProposeTimeout: 5 * time.Second, ShutdownFlush: 30 * time.Second,
		Workers: 4, ChunkWorkers: 8, RetryMin: time.Second, RetryMax: 60 * time.Second, ReuseFor: 5 * time.Minute,
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
	// warnFlush and warnPut keep a fault at one line a minute per site (I-124): a dead
	// holder would otherwise log once per dirty inode per flush pass.
	warnFlush, warnPut logx.Limiter
	cfg                Config
	log                *slog.Logger
	codecs             *ec.Pool
	Hooks              Hooks
	Events             *meta.EventRing // chunk history (nil unless dev.enabled)
	// Suspect, when set, is called for every node whose PUT was still pending when a
	// chunk stopped waiting (the straggler grace or the deadline): the prober excludes
	// it until its next good probe (docs/05 §6, I-170). Must not block.
	Suspect func(meta.NodeIdx)
	// MinCodec, when set, is the oldest codec a live active member reads (the prober's
	// view): a Commit carries the chunks' data-shard sums only when it is at least
	// meta.CodecShardSums, so a flush never fails the leader's codec gate (I-201). nil
	// means every member is current.
	MinCodec func() uint8
	putLat   latencyRing // recent PUT durations: the straggler grace follows their p95

	sem     chan struct{} // ChunkWorkers slots: chunks in memory across every flush
	buffers atomic.Int64  // chunk and shard bytes held right now
	peak    atomic.Int64

	superseded atomic.Uint64 // rewrites dropped because another started later (I-223)

	mu      sync.Mutex
	backoff map[meta.Ino]backoffState
	placed  map[meta.Hash]placedChunk  // chunks uploaded and not yet committed, for a retry (I-312)
	full    map[meta.NodeIdx]time.Time // nodes that answered ErrFull, excluded for a while
}

// placedChunk is a chunk this flusher uploaded that no Commit has registered yet.
type placedChunk struct {
	k, m  uint8
	place []meta.NodeIdx
	sums  []meta.Hash
	at    time.Time // when its PUTs were acked
}

// placedCap bounds the placed table; past it the table is cleared, costing at worst one
// re-upload per chunk. An entry is a placement and k sums, a few hundred bytes.
const placedCap = 1 << 16

// rememberPlaced records w's placement for a retry of its flush; at is when its oldest
// shard was PUT.
func (f *Flusher) rememberPlaced(w *chunkWork, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.placed) >= placedCap {
		clear(f.placed)
	}
	f.placed[w.hash] = placedChunk{k: w.k, m: w.m, place: slices.Clone(w.place), sums: slices.Clone(w.sums), at: at}
}

// recentPlaced is the placement of h this flusher uploaded within ReuseFor; an older
// entry is dropped, so its retry PUTs again and refreshes the shards' mtime (L-58).
func (f *Flusher) recentPlaced(h meta.Hash) (placedChunk, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.placed[h]
	if ok && f.clk.Now().Sub(p.at) >= f.cfg.ReuseFor {
		delete(f.placed, h)
		return placedChunk{}, false
	}
	return p, ok
}

// forgetPlaced drops committed chunks: the chunk table holds them now.
func (f *Flusher) forgetPlaced(works []*chunkWork) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range works {
		delete(f.placed, w.hash)
	}
}

type backoffState struct {
	next  time.Time
	delay time.Duration
}

// NewFlusher wires a flusher. log may be nil.
func NewFlusher(ov *Overlay, st *meta.Store, prop meta.Proposer, peers meta.Peers, shards chunk.ShardClient,
	rd *Reader, clk clock.Clock, cfg Config, log *slog.Logger,
) *Flusher {
	if log == nil {
		log = slog.Default()
	}
	return &Flusher{
		ov: ov, st: st, prop: prop, peers: peers, shards: shards, rd: rd, clk: clk, cfg: cfg,
		log: log, codecs: &ec.Pool{}, sem: make(chan struct{}, max(1, cfg.ChunkWorkers)),
		backoff: map[meta.Ino]backoffState{}, placed: map[meta.Hash]placedChunk{}, full: map[meta.NodeIdx]time.Time{},
	}
}

// Buffers reports the chunk and shard bytes the flusher holds in memory right now and
// the most it has held since start: at most ChunkWorkers chunks with their shards,
// whatever the file or the plan (docs/04 §9, I-78).
// RewritesSuperseded counts the flushes whose rewrite another client's later rewrite of
// the same file replaced, so their dirty data was dropped (docs/04 §6, I-223).
func (f *Flusher) RewritesSuperseded() uint64 { return f.superseded.Load() }

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
		f.ov.PruneRewrites() // the claim table holds only the inodes written within RewriteHold
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
					f.warnFlush.Warn(f.log, "flush failed, will retry", "ino", ino, "err", err)
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
	if f.MinCodec == nil || f.MinCodec() >= meta.CodecRewrite {
		// a writer's O_TRUNC open, or a truncate to 0 replayed or set since, starts a
		// rewrite; a later plan carries its id (docs/04 §6, I-223)
		start, id := f.ov.Rewrite(ino)
		if !start && plan.SizeSet && plan.MinSize == 0 { // a new copy, even over this gateway's last
			f.ov.MarkRewrite(ino)
			start, id = true, 0
		}
		if start {
			plan.SizeSet, plan.MinSize = true, 0
		}
		plan.rewriteStart, plan.rewrite = start, id
	}
	if plan.SizeSet {
		partial = false // truncates are never partially flushed
	}
	newSize, err := f.commitPlan(ctx, plan, partial)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			f.ov.Drop(ino)
			f.clearRetry(ino) // nothing left to retry: the backoff entry goes with the inode
			return nil
		}
		if errors.Is(err, meta.ErrSuperseded) {
			// another client's rewrite of the file started after this one: this copy ran
			// first and was replaced, so its dirty data goes; the claim stays, so the rest
			// of this copy is refused the same way rather than mixed into theirs (I-223)
			f.ov.Drop(ino)
			f.clearRetry(ino)
			f.superseded.Add(1)
			if f.log != nil {
				f.log.Info("rewrite superseded by a later one, dirty data dropped", "ino", ino)
			}
			return nil
		}
		f.ov.AbortFlush(plan)
		f.ov.NoteFlush(ino, err) // a WRITE at the cap fails at once behind this (L-189)
		return err
	}
	f.ov.NoteFlush(ino, nil) // before CommitFlushed, which may evict the entry
	f.restoreTimes(ctx, ino)
	return f.ov.CommitFlushed(plan, newSize)
}

// restoreTimes sets again the times a client set on ino after its last write, which the
// Commit just stamped over with its own (I-230): a copy's source times, set as its last
// step, outlive the flush. A failure leaves the flush's time and is logged; the data is
// committed either way.
func (f *Flusher) restoreTimes(ctx context.Context, ino meta.Ino) {
	atime, mtime, ok := f.ov.UserTimes(ino)
	if !ok {
		return
	}
	sa := &meta.SetAttr{Ino: ino, Mask: meta.AttrAtime | meta.AttrMtime, Atime: atime, Mtime: mtime, Now: f.clk.Now().UnixNano()}
	if _, _, err := f.prop.Propose(ctx, f.actorCommand(f.ov.Actor(ino), meta.Command{Op: meta.OpSetAttr, SetAttr: sa})); err != nil && f.log != nil {
		f.log.Warn("a client's file times were not set again after the flush", "ino", ino, "err", err)
	}
}

// actorCommand stamps the writer on a flush's command (docs/21 §4, I-353): a Commit or
// the SetAttr that restores a client's times acts for the user who wrote; a cluster
// whose members read codec 13 gets none, as the gateway sends none.
func (f *Flusher) actorCommand(actor meta.UserIdx, cmd meta.Command) meta.Command {
	if !f.st.Enforcing() || (f.MinCodec != nil && f.MinCodec() < meta.CodecUsers) {
		return cmd // before the first user, and while a member reads an older codec, the wire carries what it did
	}
	if actor == 0 {
		// a write with no actor is the node's own (every user's view stamps theirs, and
		// nobody's view writes nothing): the stats, audit, thumbnail and index writers
		// under /.tailraid, whose Commit the FSM takes as System and nowhere else (I-349)
		cmd.System = true
		return cmd
	}
	cmd.Actor = actor
	return cmd
}

// carriesSums reports whether a Commit records any chunk's shard sums (a codec-5 command).
func carriesSums(c *meta.Commit) bool {
	for _, d := range c.NewChunks {
		if d.Sums != nil {
			return true
		}
	}
	return false
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
	sums     []meta.Hash // the data shards' hashes, from the encode (I-201)
	uploaded bool
}

// commitPlan runs steps 2–11 for a plan, including rebases, and returns the committed size.
func (f *Flusher) commitPlan(ctx context.Context, plan *FlushPlan, partial bool) (uint64, error) {
	forceUpload := false
	for range 16 {
		base, ok := f.st.GetInodeAny(plan.Ino) // a file deleted while dirty commits into its trash entry (L-203)
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
		var (
			cmd    *meta.Commit
			res    meta.Result
			index  uint64
			noSums bool
		)
		for {
			cmd = f.buildCommit(plan, base, works, partial || limited, noSums)
			if f.Hooks.BeforeCommit != nil {
				f.Hooks.BeforeCommit()
			}
			pctx, cancel := context.WithTimeout(ctx, f.cfg.ProposeTimeout)
			var perr error
			res, index, perr = f.prop.Propose(pctx, f.actorCommand(plan.Actor, meta.Command{Op: meta.OpCommit, Commit: cmd}))
			cancel()
			if perr != nil {
				return 0, fmt.Errorf("propose commit: %w", perr)
			}
			if errors.Is(res.Err, meta.ErrCodec) && !noSums && (carriesSums(cmd) || cmd.Rewrite != 0 || cmd.RewriteStart) {
				// the leader sees a member older than this gateway does (a rolling
				// upgrade): the same chunks without their sums, which the backfill
				// records later (I-219), and as a plain write
				noSums = true
				continue
			}
			break
		}
		switch {
		case res.Err == nil:
			if cmd.RewriteStart {
				f.ov.SetRewrite(plan.Ino, res.Version) // the id later Commits carry (I-223)
			} else {
				f.ov.ForgetPendingRewrite(plan.Ino) // committed plain: nothing left to start
			}
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
			f.forgetPlaced(works)
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
		case errors.Is(res.Err, meta.ErrSuperseded):
			return 0, meta.ErrSuperseded
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
		// chunks the base holds past the truncation point must be rewritten as holes;
		// chunks past the base list are holes already (the FSM grows the list with
		// HashZero), so a truncate followed by a far seek costs no write per index
		for i := (plan.MinSize + cs - 1) / cs; i < n && int(i) < len(base.Chunks); i++ {
			if !touched[i] && base.Chunks[i] != meta.HashZero {
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
	slices.Sort(idxs)
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
	started := 0
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			i = n
			continue
		}
		started++
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
	if first == nil && started < n {
		// the caller's context ended before every item started: the work is incomplete,
		// which a nil error would hide (a flush cancelled at shutdown, nil works)
		return ctx.Err()
	}
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
	if !forceUpload {
		if p, ok := f.recentPlaced(w.hash); ok {
			// a retry of a flush that placed this chunk moments ago: the shards are young
			// and on their holders; HEAD them and PUT only what is missing (I-312)
			keep, present := f.presentSlots(ctx, meta.Chunk{Hash: w.hash, K: p.k, M: p.m, Placement: p.place})
			w.k, w.m = p.k, p.m
			if present == int(p.k)+int(p.m) {
				w.place, w.sums, w.uploaded = keep, slices.Clone(p.sums), true
				f.Events.Record(w.hash, meta.EvUploaded, fmt.Sprintf("%d+%d reused placement %v", w.k, w.m, keep))
				return nil
			}
			if present >= int(p.k) {
				if err := f.upload(ctx, w, data, keep); err != nil {
					return err
				}
				f.rememberPlaced(w, p.at) // the kept shards are as old as before
				return nil
			}
		}
	}
	vol := f.st.Volume()
	w.k, w.m = vol.K, vol.M
	if meta.IsSmallChunk(uint32(len(data)), vol.ChunkSize) {
		w.k = 1 // replication for small chunks (docs/03)
	}
	if err := f.upload(ctx, w, data, nil); err != nil {
		return err
	}
	f.rememberPlaced(w, f.clk.Now())
	return nil
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
	w.sums = make([]meta.Hash, int(w.k)) // a pure function of the chunk's bytes and (k, m), I-201
	for j := range w.sums {
		w.sums[j] = sha256Sum(shards[j])
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
	meta.FillPlacement(w.hash, place, f.candidates(held), int(w.k), meta.DomainsOf(f.st.Nodes()))
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
		meta.FillPlacement(w.hash, place, f.candidates(exclude), int(w.k), meta.DomainsOf(f.st.Nodes()))
		maps.Copy(failed, f.putSlots(ctx, w, shards, place, acked))
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
		if (len(failed) > 0 && allFull(failed)) || !meta.HasTargets(f.st.Nodes()) {
			// ENOSPC to the client, not EIO: every holder is full (I-79), or the volume has
			// no disk to hold anything (docs/14 §9, I-120)
			err = fmt.Errorf("%w: %w", err, chunk.ErrFull)
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

// StragglerGrace is how long a chunk with k acks waits for its remaining PUTs: four
// times the p95 of recent PUT durations, within [floor, deadline] (docs/04 §4 step 9,
// I-169).
func StragglerGrace(p95, floor, deadline time.Duration) time.Duration {
	return min(max(4*p95, floor), deadline)
}

// putSlots PUTs every unacked slot with a non-zero node in parallel and waits until ≥k
// acks and (all done, or the deadline, or the straggler grace from the k-th ack). It
// marks acked slots and returns the failed ones with their errors. Stragglers are
// cancelled and awaited before it returns, so no goroutine keeps a shard buffer alive
// past its chunk (L-190, I-83), and their nodes are marked suspect (I-170).
func (f *Flusher) putSlots(ctx context.Context, w *chunkWork, shards [][]byte, place []meta.NodeIdx, acked []bool) map[int]error {
	type res struct {
		slot int
		err  error
		took time.Duration
	}
	pctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	out := make(chan res, len(place))
	inflight := map[int]bool{}
	for i, n := range place {
		if n == 0 || acked[i] {
			continue
		}
		inflight[i] = true
		wg.Add(1)
		go func(slot int, node meta.NodeIdx) {
			defer wg.Done()
			start := f.clk.Now()
			err := f.shards.Put(pctx, node, chunk.Key(w.hash, w.k, w.m, slot), shards[slot])
			out <- res{slot, err, f.clk.Since(start)}
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
	// still cut the wait short when that ack arrives. The grace is armed at the k-th ack
	// and ends the wait the same way (L-209).
	deadline := f.clk.After(f.cfg.PutDeadline)
	var grace <-chan time.Time
	expired := false
	armGrace := func() {
		if grace == nil && acks >= int(w.k) && len(inflight) > 0 {
			p95, _ := f.putLat.p95()
			grace = f.clk.After(StragglerGrace(p95, f.cfg.StragglerMin, f.cfg.PutDeadline))
		}
	}
	armGrace()
	for len(inflight) > 0 {
		if expired && acks >= int(w.k) {
			break
		}
		var r res
		select {
		case r = <-out:
		case <-deadline:
			expired, deadline = true, nil
			continue
		case <-grace:
			expired, grace = true, nil
			continue
		}
		delete(inflight, r.slot)
		if r.err != nil {
			failed[r.slot] = r.err
			if errors.Is(r.err, chunk.ErrFull) {
				f.mu.Lock()
				f.full[place[r.slot]] = f.clk.Now().Add(time.Minute)
				f.mu.Unlock()
			}
			f.warnPut.Warn(f.log, "shard put failed", "node", place[r.slot], "slot", r.slot, "err", r.err)
			f.Events.Record(w.hash, meta.EvPutFailed, fmt.Sprintf("slot %d node %d: %v", r.slot, place[r.slot], r.err))
			continue
		}
		acked[r.slot] = true
		acks++
		f.putLat.add(r.took)
		armGrace()
	}
	for slot := range inflight { // stragglers: still pending at the grace or the deadline
		f.Events.Record(w.hash, meta.EvPutFailed, fmt.Sprintf("slot %d node %d: still pending, marked suspect", slot, place[slot]))
		if f.Suspect != nil {
			f.Suspect(place[slot])
		}
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
	res, index, err := f.prop.Propose(ctx, meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: w.hash, K: w.k, M: w.m, Placement: w.place, Expect: c.Placement}})
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
func (f *Flusher) buildCommit(plan *FlushPlan, base meta.Inode, works []*chunkWork, partial, noSums bool) *meta.Commit {
	cs := uint64(f.st.Volume().ChunkSize)
	cmd := &meta.Commit{Ino: plan.Ino, BaseVersion: base.Version, Now: f.clk.Now().UnixNano()}
	if !noSums { // noSums also marks a leader that refused codec 9: then a plain write
		cmd.RewriteStart, cmd.Rewrite = plan.rewriteStart, plan.rewrite
	}
	seen := map[meta.Hash]bool{}
	// else the backfill records them (docs/06 §4)
	withSums := !noSums && (f.MinCodec == nil || f.MinCodec() >= meta.CodecShardSums)
	var lastFull uint64
	for _, w := range works {
		cmd.Writes = append(cmd.Writes, meta.ChunkWrite{Idx: uint32(w.idx), Hash: w.hash})
		if !w.hole && w.uploaded && !seen[w.hash] {
			seen[w.hash] = true
			d := meta.ChunkDesc{Hash: w.hash, Size: w.size, K: w.k, M: w.m, Placement: w.place}
			if withSums {
				d.Sums = w.sums
			}
			cmd.NewChunks = append(cmd.NewChunks, d)
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
		cmd.NewSize = max(plan.Size,
			// truncate-up without writes
			max(plan.MinSize, plan.Extents.maxEnd()))
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
