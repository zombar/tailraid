// Package repair restores redundancy and reclaims space (docs/06): the leader-only GC
// worker and repair scanner, the scrub sampler, and the per-node orphan sweep.
package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/logx"
	"github.com/zombar/tailraid/internal/meta"
)

// Liveness is the prober view the scanner needs (cluster.Prober).
type Liveness interface {
	Self() meta.NodeIdx
	Live() []meta.NodeIdx
	IsLive(idx meta.NodeIdx) bool
	DeadFor(idx meta.NodeIdx) time.Duration
}

// Shards is the transport the repairer uses (rpc.Client).
type Shards interface {
	chunk.ShardClient
	Has(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) (bool, error)
	DeleteFenced(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, index uint64) error
}

// ChunkReader decodes a chunk from k shards (vfs.Reader).
type ChunkReader interface {
	ReadChunk(ctx context.Context, h meta.Hash) ([]byte, error)
}

// Config tunes the repairer.
type Config struct {
	FillAfter    time.Duration // dead-node hold-down before re-placing its slots
	ScrubShare   int           // 1 in ScrubShare chunks verified per pass (0 = none, 1 = all)
	S3ScrubBytes int64         // bucket bytes the scrub may fetch per day to verify objects (0 = HEAD only)
	RateBytes    int64         // shard bytes per second moved by repair (0 = unlimited)
	Grace        time.Duration // GC grace
	GCInterval   time.Duration
	ScanInterval time.Duration
	ReportTTL    time.Duration // reported bad slots expire after this
	Retention    time.Duration // the trash's retention (docs/06 §2, trash.retention); 0 = never purged on its own
	// Capacity rebalance within a host (docs/14 §7, I-172): a disk more than
	// RebalanceSkew (a fraction) above its share of its host's slots has slots moved to
	// a disk of the same host below its share, at most RebalanceBatch chunks per pass
	// (0 disables it).
	RebalanceSkew  float64
	RebalanceBatch int
	// Workers is how many chunks a pass repairs at once (docs/06 §4, I-174): each chunk's
	// steps stay in sequence, chunks overlap; at most MaxWorkers, below 1 means 1.
	Workers int
}

// DefaultRetention is how long a deleted entry waits in the trash (docs/12 §10).
const DefaultRetention = 30 * 24 * time.Hour

// MaxWorkers bounds Config.Workers (`repair.workers`).
const MaxWorkers = 64

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		FillAfter: 10 * time.Minute, ScrubShare: 100, S3ScrubBytes: 1 << 30, RateBytes: 50 << 20, Grace: 10 * time.Minute,
		GCInterval: 10 * time.Minute, ScanInterval: time.Minute, ReportTTL: 10 * time.Minute, Retention: DefaultRetention,
		RebalanceSkew: meta.DefaultRebalanceSkew, RebalanceBatch: 64, Workers: 4,
	}
}

// Progress is the pass under way: what `status` shows as `in_progress` while Last still
// reports the pass before it (docs/12 §8).
type Progress struct {
	Pass       int       `json:"pass"`
	Started    time.Time `json:"started"`
	Scanned    int       `json:"scanned"`
	Total      int       `json:"total"`
	InFlight   int       `json:"in_flight"`
	Workers    int       `json:"workers"`
	BytesMoved int64     `json:"bytes_moved"`
}

// Stats summarises one scan pass (admin.RepairStatus).
type Stats struct {
	Scanned      int   `json:"scanned"`
	Repaired     int   `json:"repaired"`     // chunks brought to full placement
	Partial      int   `json:"partial"`      // chunks improved but still degraded
	Unrepairable int   `json:"unrepairable"` // < k good live slots
	Deferred     int   `json:"deferred"`     // policy migration not committed: fewer than k acks
	Migrated     int   `json:"migrated"`     // moved to the current (k,m)
	PolicyStale  int   `json:"policy_stale"` // still on an older (k,m) after the pass
	Scrubbed     int   `json:"scrubbed"`
	ScrubBad     int   `json:"scrub_bad"`  // slots found corrupt by scrub
	Rebalanced   int   `json:"rebalanced"` // slots moved between disks of one host (docs/14 §7)
	BytesMoved   int64 `json:"bytes_moved"`
	Errors       int   `json:"errors"`
}

// add sums o into s: a pass's statistics are the sum over its chunks, whatever the overlap.
func (s *Stats) add(o Stats) {
	s.Scanned += o.Scanned
	s.Repaired += o.Repaired
	s.Partial += o.Partial
	s.Unrepairable += o.Unrepairable
	s.Deferred += o.Deferred
	s.Migrated += o.Migrated
	s.PolicyStale += o.PolicyStale
	s.Scrubbed += o.Scrubbed
	s.ScrubBad += o.ScrubBad
	s.Rebalanced += o.Rebalanced
	s.BytesMoved += o.BytesMoved
	s.Errors += o.Errors
}

// Repairer is the leader's scanner.
type Repairer struct {
	// warnRepair and warnPut keep a fault at one line a minute per site (I-124)
	warnRepair, warnPut logx.Limiter
	st                  *meta.Store
	prop                meta.Proposer
	live                Liveness
	shards              Shards
	rd                  ChunkReader
	clk                 clock.Clock
	cfg                 Config
	log                 *slog.Logger
	codecs              *ec.Pool
	limit               *limiter
	s3scrub             *budget         // bucket bytes the scrub may fetch (docs/06 §5, I-151)
	Events              *meta.EventRing // chunk history (nil unless dev.enabled)

	scanMu   sync.Mutex // one pass at a time: `check --scrub` never overlaps the background loop
	mu       sync.Mutex
	reported map[chunk.ShardKey]time.Time // reader bad-slot reports
	last     Stats
	lastAt   time.Time
	pass     int
	progress Progress // the pass under way, while running
	running  bool
}

// New wires a repairer. log may be nil.
func New(st *meta.Store, prop meta.Proposer, live Liveness, shards Shards, rd ChunkReader, clk clock.Clock, cfg Config, log *slog.Logger) *Repairer {
	if log == nil {
		log = slog.Default()
	}
	return &Repairer{
		st: st, prop: prop, live: live, shards: shards, rd: rd, clk: clk, cfg: cfg, log: log, codecs: &ec.Pool{},
		limit: newLimiter(cfg.RateBytes, clk), s3scrub: newBudget(cfg.S3ScrubBytes, 24*time.Hour, clk), reported: map[chunk.ShardKey]time.Time{},
	}
}

// ReportBad records a reader's bad-slot report (docs/06 §4 case 5); bounded and expiring.
func (r *Repairer) ReportBad(key chunk.ShardKey, _ meta.NodeIdx) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reported) >= 100_000 {
		return
	}
	r.reported[key] = r.clk.Now()
}

// Reported returns the number of outstanding reader bad-slot reports.
func (r *Repairer) Reported() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reported)
}

// Last returns the most recent pass statistics and when it ran.
func (r *Repairer) Last() (Stats, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last, r.lastAt
}

// Progress returns the pass under way and whether one is running.
func (r *Repairer) Progress() (Progress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.progress, r.running
}

// Run scans every ScanInterval until ctx is done; isLeader gates each pass.
func (r *Repairer) Run(ctx context.Context, isLeader func() bool) {
	t := r.clk.NewTimer(r.cfg.ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
		}
		if isLeader() {
			if _, err := r.Scan(ctx); err != nil {
				r.log.Warn("repair scan failed", "err", err)
			}
		}
		t.Reset(r.cfg.ScanInterval)
	}
}

// classify derives the chunk's state with the shared meta.ChunkState (docs/06 §4 cases
// 1–5, I-56) and returns the bad slots, the kept slots (0 where bad), whether a policy
// migration is due and how many slots a read can use right now.
func (r *Repairer) classify(c meta.Chunk, vol meta.Volume, nodes map[meta.NodeIdx]meta.Node, reported map[int]bool) (bad []meta.SlotIssue, kept []meta.NodeIdx, migrate bool, readable int) {
	h := meta.ChunkState(c, vol, meta.HealthInput{Nodes: nodes, Live: r.live, FillAfter: r.cfg.FillAfter, Reported: reported})
	kept = append([]meta.NodeIdx(nil), c.Placement...)
	for _, b := range h.Bad {
		kept[b.Idx] = 0
	}
	return h.Bad, kept, h.PolicyStale, h.Readable
}

// autoPolicy proposes the policy the member table calls for (docs/03, I-75): only when
// it differs from the volume's and a full placement is possible now; otherwise the change
// waits for a later pass. Returns the volume the pass scans with.
func (r *Repairer) autoPolicy(ctx context.Context, nodes []meta.Node, vol meta.Volume) meta.Volume {
	k, m := meta.AutoPolicy(nodes)
	if k == vol.K && m == vol.M {
		return vol
	}
	if err := meta.PolicyAllowed(nodes, r.live, k, m); err != nil {
		r.log.Info("auto policy deferred", "want", fmt.Sprintf("%d+%d", k, m), "reason", err)
		return vol
	}
	res, index, err := r.prop.Propose(ctx, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: k, M: m, Now: r.clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		r.log.Warn("auto policy proposal failed, will retry", "err", err)
		return vol
	}
	if err := r.st.WaitApplied(ctx, index); err != nil {
		return vol
	}
	r.log.Info("auto policy", "from", fmt.Sprintf("%d+%d", vol.K, vol.M), "to", fmt.Sprintf("%d+%d", k, m))
	return r.st.Volume()
}

// Config returns the repairer's configuration (status derives states with its fill_after).
func (r *Repairer) Config() Config { return r.cfg }

// Scan performs one pass: repair, migrate, scrub (docs/06 §4–5).
func (r *Repairer) Scan(ctx context.Context) (Stats, error) { return r.scan(ctx, r.cfg.ScrubShare) }

// Scrub is one pass that verifies every chunk (`check --scrub`), with the configured
// hold-downs and rate limit, serialized with the background passes.
func (r *Repairer) Scrub(ctx context.Context) (Stats, error) { return r.scan(ctx, 1) }

func (r *Repairer) scan(ctx context.Context, scrubShare int) (Stats, error) {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	var st Stats
	vol := r.st.Volume()
	nodes := map[meta.NodeIdx]meta.Node{}
	all := r.st.Nodes()
	if vol.Auto {
		vol = r.autoPolicy(ctx, all, vol)
	}
	for i := range all {
		nodes[all[i].Idx] = all[i]
	}
	r.mu.Lock()
	r.pass++
	pass := r.pass
	now := r.clk.Now()
	reportedKeys := map[meta.Hash]map[int]bool{}
	for k, at := range r.reported {
		if now.Sub(at) > r.cfg.ReportTTL {
			delete(r.reported, k)
			continue
		}
		if h, _, _, idx, err := chunk.ParseKey(k); err == nil {
			if reportedKeys[h] == nil {
				reportedKeys[h] = map[int]bool{}
			}
			reportedKeys[h][idx] = true
		}
	}
	r.mu.Unlock()

	// policy migration order: fewest effective parity first
	var work []meta.Chunk
	r.st.ForEachChunk(func(c meta.Chunk) bool {
		if c.Refs > 0 {
			work = append(work, c)
		}
		return true
	})
	sort.SliceStable(work, func(i, j int) bool { return work[i].M < work[j].M })
	slots := map[meta.NodeIdx]int{} // per target, for the capacity rebalance (docs/14 §7)
	for i := range work {
		for _, s := range work[i].Placement {
			if s != 0 {
				slots[s]++
			}
		}
	}

	workers := max(1, r.cfg.Workers)
	r.mu.Lock()
	r.progress = Progress{Pass: pass, Started: now, Total: len(work), Workers: workers}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()
	// Every shard request of the pass is background work: a node serving it keeps half its
	// slots for client-path reads and flushes (I-175). At most Workers chunks are in flight
	// at once (I-174); a chunk's own steps stay in sequence, and the statistics are sums,
	// the same whatever the overlap. A cancelled pass starts no more chunks and returns
	// once the ones in flight end.
	ctx = chunk.Background(ctx)
	var (
		wg  sync.WaitGroup
		smu sync.Mutex
		sem = make(chan struct{}, workers)
	)
	for _, c := range work {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(c meta.Chunk) {
			defer wg.Done()
			defer func() { <-sem }()
			r.mu.Lock()
			r.progress.InFlight++
			r.mu.Unlock()
			one := r.scanOne(ctx, c, vol, nodes, reportedKeys[c.Hash], pass, scrubShare)
			r.mu.Lock()
			r.progress.InFlight--
			r.progress.Scanned++
			r.progress.BytesMoved += one.BytesMoved
			r.mu.Unlock()
			smu.Lock()
			st.add(one)
			smu.Unlock()
		}(c)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return st, ctx.Err()
	}
	st.Rebalanced, st.BytesMoved = r.rebalance(ctx, &st, all, slots, len(work))
	r.mu.Lock()
	r.last, r.lastAt = st, r.clk.Now()
	r.mu.Unlock()
	return st, nil
}

// scanOne classifies one chunk and repairs, migrates or scrubs it (docs/06 §4 cases
// 1–6), returning what it did as that chunk's share of the pass statistics.
func (r *Repairer) scanOne(ctx context.Context, c meta.Chunk, vol meta.Volume, nodes map[meta.NodeIdx]meta.Node, reportedIdx map[int]bool, pass, scrubShare int) (st Stats) {
	st.Scanned++
	// re-verify reported slots with a HEAD; a present shard is not bad (docs/06 §4 case 5)
	reported := map[int]bool{}
	for idx := range reportedIdx {
		if idx < len(c.Placement) && c.Placement[idx] != 0 && r.live.IsLive(c.Placement[idx]) {
			has, err := r.shards.Has(ctx, c.Placement[idx], chunk.Key(c.Hash, c.K, c.M, idx))
			if err == nil && has {
				r.mu.Lock()
				delete(r.reported, chunk.Key(c.Hash, c.K, c.M, idx))
				r.mu.Unlock()
				continue
			}
			reported[idx] = true
		}
	}
	bad, kept, migrate, readable := r.classify(c, vol, nodes, reported)
	if len(bad) == 0 && !migrate {
		if scrubShare <= 0 || int(c.Hash[0])%scrubShare != pass%scrubShare {
			return st
		}
		st.Scrubbed++
		corrupt, err := r.scrub(ctx, c)
		if err != nil {
			st.Errors++
			return st
		}
		if len(corrupt) == 0 {
			r.Events.Record(c.Hash, meta.EvScrubOK, "")
			return st
		}
		st.ScrubBad += len(corrupt)
		r.Events.Record(c.Hash, meta.EvScrubBad, fmt.Sprintf("slots %v", corrupt))
		for _, i := range corrupt {
			bad = append(bad, meta.SlotIssue{Idx: i, Reason: "scrub"})
			if kept[i] != 0 && r.live.IsLive(kept[i]) {
				readable--
			}
			kept[i] = 0
		}
	}
	if migrate {
		st.PolicyStale++
	}
	if readable < int(c.K) {
		st.Unrepairable++
		r.Events.Record(c.Hash, meta.EvUnrepairable, fmt.Sprintf("%d readable slots < k=%d", readable, c.K))
		return st
	}
	outcome, err := r.repairOne(ctx, c, bad, kept, migrate, vol)
	if err != nil {
		st.Errors++
		r.warnRepair.Warn(r.log, "repair failed", "chunk", fmt.Sprintf("%x", c.Hash[:8]), "err", err)
		return st
	}
	st.BytesMoved += outcome.bytes
	switch {
	case !outcome.changed && migrate:
		st.Deferred++ // fewer than k acks: the old encoding stays committed, retried next pass
	case !outcome.changed:
		// no live spare could take the bad slots; counted as still degraded
	case migrate:
		// the chunk is on the new policy now; with missing slots it is also partial
		st.Migrated++
		st.PolicyStale--
		if !outcome.full {
			st.Partial++
		}
		r.Events.Record(c.Hash, meta.EvMigrated, fmt.Sprintf("%d+%d -> %d+%d", c.K, c.M, vol.K, vol.M))
	case outcome.full:
		st.Repaired++
		r.Events.Record(c.Hash, meta.EvRepaired, fmt.Sprintf("%d bad slots", len(bad)))
	default:
		st.Partial++
		r.Events.Record(c.Hash, meta.EvPartial, fmt.Sprintf("%d bad slots", len(bad)))
	}
	return st
}

// rebalance evens out each host's disks by capacity (docs/14 §7, I-172): for every
// move meta.RebalanceMoves lists, chunks holding a slot on the fuller disk and none on
// the emptier one get that slot moved there, at most RebalanceBatch per pass. It returns
// the slots moved and the bytes total including st.BytesMoved.
func (r *Repairer) rebalance(ctx context.Context, st *Stats, nodes []meta.Node, slots map[meta.NodeIdx]int, chunks int) (int, int64) {
	moved, total := 0, st.BytesMoved
	if r.cfg.RebalanceBatch <= 0 {
		return 0, total
	}
	for _, mv := range meta.RebalanceMoves(nodes, slots, chunks, r.cfg.RebalanceSkew, r.live.IsLive) {
		n := min(mv.N, r.cfg.RebalanceBatch-moved)
		if n <= 0 {
			break
		}
		var cands []meta.Chunk
		r.st.ForEachChunk(func(c meta.Chunk) bool {
			if c.Refs == 0 || slices.Contains(c.Placement, mv.To) || !slices.Contains(c.Placement, mv.From) {
				return true
			}
			cands = append(cands, c)
			return len(cands) < n
		})
		for _, c := range cands {
			if ctx.Err() != nil {
				return moved, total
			}
			b, err := r.moveSlot(ctx, c, slices.Index(c.Placement, mv.From), mv.To)
			total += b
			if err != nil {
				st.Errors++
				r.warnRepair.Warn(r.log, "rebalance failed", "chunk", fmt.Sprintf("%x", c.Hash[:8]), "err", err)
				continue
			}
			moved++
			r.Events.Record(c.Hash, meta.EvRepaired, fmt.Sprintf("rebalance: slot moved %d -> %d within host %d", mv.From, mv.To, mv.Host))
		}
	}
	return moved, total
}

// moveSlot re-places one slot of c onto `to`: PUT the shard there, commit the placement
// (Expect fences a concurrent change, I-102), then the fenced DELETE of the old shard,
// so the chunk never has fewer readable slots than before (I-65). Returns the bytes put.
func (r *Repairer) moveSlot(ctx context.Context, c meta.Chunk, slot int, to meta.NodeIdx) (int64, error) {
	if slot < 0 {
		return 0, errors.New("slot not in the placement")
	}
	data, err := r.rd.ReadChunk(ctx, c.Hash)
	if err != nil {
		return 0, fmt.Errorf("read: %w", err)
	}
	codec, err := r.codecs.Get(c.K, c.M)
	if err != nil {
		return 0, err
	}
	shards, err := codec.Encode(data)
	if err != nil {
		return 0, err
	}
	key := chunk.Key(c.Hash, c.K, c.M, slot)
	r.limit.wait(ctx, int64(len(shards[slot])))
	if err := r.shards.Put(ctx, to, key, shards[slot]); err != nil {
		return 0, fmt.Errorf("put: %w", err)
	}
	moved := int64(len(shards[slot]))
	newPlace := slices.Clone(c.Placement)
	newPlace[slot] = to
	res, index, err := r.prop.Propose(ctx, meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: c.K, M: c.M, Placement: newPlace, Expect: c.Placement}})
	if err == nil {
		err = res.Err
	}
	if errors.Is(err, meta.ErrConflict) {
		return moved, nil // re-placed meanwhile: the next pass plans afresh; the shard is an orphan the sweep clears
	}
	if err != nil {
		return moved, fmt.Errorf("set placement: %w", err)
	}
	if err := r.st.WaitApplied(ctx, index); err != nil {
		return moved, err
	}
	if old := c.Placement[slot]; r.live.IsLive(old) {
		if err := r.shards.DeleteFenced(ctx, old, key, index); err != nil {
			r.log.Debug("stale shard delete refused", "node", old, "slot", slot, "err", err)
		}
	}
	return moved, nil
}

type outcome struct {
	changed bool // a new placement was committed
	full    bool
	bytes   int64
}

// repairOne re-places the bad slots (or every slot for a migration), commits the new
// placement and only then deletes superseded shards (docs/06 §4 steps 1–6).
func (r *Repairer) repairOne(ctx context.Context, c meta.Chunk, bad []meta.SlotIssue, kept []meta.NodeIdx, migrate bool, vol meta.Volume) (outcome, error) {
	data, err := r.rd.ReadChunk(ctx, c.Hash)
	if err != nil {
		return outcome{}, fmt.Errorf("read: %w", err)
	}
	k, m := c.K, c.M
	if migrate {
		k, m = meta.TargetPolicy(c, vol)
	}
	codec, err := r.codecs.Get(k, m)
	if err != nil {
		return outcome{}, err
	}
	shards, err := codec.Encode(data)
	if err != nil {
		return outcome{}, err
	}
	total := int(k) + int(m)
	newPlace := make([]meta.NodeIdx, total)
	var held []meta.NodeIdx
	if !migrate {
		copy(newPlace, kept)
		for _, n := range kept {
			if n != 0 {
				held = append(held, n)
			}
		}
	}
	live := meta.LiveSet(r.live.Live())
	nodes := r.st.Nodes()
	meta.FillPlacement(c.Hash, newPlace, meta.ExcludeIdx(meta.Candidates(nodes, func(n meta.NodeIdx) bool { return live[n] }), held), int(k), meta.DomainsOf(nodes))
	// A slot whose shard is still served (live draining holder) keeps its node until a
	// replacement is placed: a repair never lowers the readable slot count (I-65, L-184).
	// unchanged marks slots that need no PUT: kept ones and retained ones.
	unchanged := make([]bool, total)
	for i := range unchanged {
		unchanged[i] = !migrate && i < len(kept) && kept[i] != 0
	}
	if !migrate {
		for _, b := range bad {
			if b.Readable && newPlace[b.Idx] == 0 {
				newPlace[b.Idx] = c.Placement[b.Idx]
				unchanged[b.Idx] = true
			}
		}
	}
	var moved int64
	acked := 0
	for i, n := range newPlace {
		if n == 0 {
			continue
		}
		if unchanged[i] {
			acked++
			continue
		}
		r.limit.wait(ctx, int64(len(shards[i])))
		if err := r.shards.Put(ctx, n, chunk.Key(c.Hash, k, m, i), shards[i]); err != nil {
			r.warnPut.Warn(r.log, "repair put failed", "node", n, "slot", i, "err", err)
			newPlace[i] = 0
			continue
		}
		moved += int64(len(shards[i]))
		acked++
	}
	if migrate && acked < int(k) {
		return outcome{bytes: moved}, nil // keep the old encoding committed; try again next pass
	}
	if !migrate {
		stayed := 0
		for _, u := range unchanged {
			if u {
				stayed++
			}
		}
		if acked == stayed {
			return outcome{bytes: moved}, nil // nothing placeable improved; nothing to commit (L-185)
		}
	}
	res, index, err := r.prop.Propose(ctx, meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: k, M: m, Placement: newPlace, Expect: c.Placement}})
	if err == nil {
		err = res.Err
	}
	if errors.Is(err, meta.ErrConflict) {
		// re-placed by a flush or a drain since this pass read it (I-102): the next pass
		// plans from the new placement; the shards just put are orphans the sweep clears
		r.log.Info("placement changed under repair, skipping this pass", "chunk", fmt.Sprintf("%x", c.Hash[:8]))
		return outcome{bytes: moved}, nil
	}
	if err != nil {
		return outcome{bytes: moved}, fmt.Errorf("set placement: %w", err)
	}
	if err := r.st.WaitApplied(ctx, index); err != nil {
		return outcome{bytes: moved}, err
	}
	// superseded shards: old keys after a migration, or slots that moved off live nodes
	for i, old := range c.Placement {
		if old == 0 || !r.live.IsLive(old) {
			continue // a dead node sweeps its own stale shards when it returns
		}
		var stale bool
		if migrate {
			stale = true
		} else {
			stale = i < len(newPlace) && newPlace[i] != old
		}
		if stale {
			if err := r.shards.DeleteFenced(ctx, old, chunk.Key(c.Hash, c.K, c.M, i), index); err != nil {
				r.log.Debug("stale shard delete refused", "node", old, "slot", i, "err", err)
			}
		}
	}
	full := true
	for _, n := range newPlace {
		if n == 0 {
			full = false
		}
	}
	return outcome{changed: true, full: full, bytes: moved}, nil
}

// scrub fetches every shard of c and returns the slots whose content is wrong. A slot on
// an S3 node is checked for presence (HEAD) unless the daily fetch budget is in credit:
// fetching every one would download the bucket's share of the volume every ~100 passes
// in billed egress, so the bucket is verified over time at a bounded cost (docs/06 §5,
// I-151). A fetched object fails like a disk shard: ErrCorrupt from the hash check.
func (r *Repairer) scrub(ctx context.Context, c meta.Chunk) ([]int, error) {
	total := int(c.K) + int(c.M)
	got := make([][]byte, total)
	var corrupt []int
	for i, n := range c.Placement {
		if n == 0 || !r.live.IsLive(n) {
			continue
		}
		if nd, ok := r.st.Node(n); ok && nd.Kind == meta.KindS3 && !r.s3scrub.take(int64(c.Size)/int64(c.K)+chunk.HeaderSize) {
			if has, err := r.shards.Has(ctx, n, chunk.Key(c.Hash, c.K, c.M, i)); err == nil && !has {
				corrupt = append(corrupt, i)
			}
			continue
		}
		b, err := r.shards.Get(ctx, n, chunk.Key(c.Hash, c.K, c.M, i))
		if err != nil {
			if errors.Is(err, chunk.ErrNotFound) || errors.Is(err, chunk.ErrCorrupt) {
				corrupt = append(corrupt, i)
			}
			continue
		}
		got[i] = b
	}
	codec, err := r.codecs.Get(c.K, c.M)
	if err != nil {
		return corrupt, err
	}
	// decode from the shards just fetched rather than fetching them again through the
	// reader: a verified decode needs any k of them, and the hash check below is the
	// same one the reader applies
	data, err := codec.Decode(append([][]byte(nil), got...), int(c.Size))
	if err != nil || sha256.Sum256(data) != c.Hash {
		data, err = r.rd.ReadChunk(ctx, c.Hash) // a bad majority: let the reader hedge across holders
		if err != nil {
			return corrupt, err
		}
	}
	want, err := codec.Encode(data)
	if err != nil {
		return corrupt, err
	}
	for i := range got {
		if got[i] != nil && !bytes.Equal(got[i], want[i]) {
			corrupt = append(corrupt, i)
		}
	}
	sort.Ints(corrupt)
	return corrupt, nil
}

// --- daily budget ----------------------------------------------------------------

// budget is a token bucket for work that is allowed, not delayed: take reports whether n
// is within credit now, refilling perPeriod tokens over period and holding at most one
// period's worth. Credit may go negative on a take, so a budget smaller than one item
// still admits one item per period.
type budget struct {
	perPeriod int64
	period    time.Duration
	clk       clock.Clock
	mu        sync.Mutex
	tokens    int64
	last      time.Time
}

func newBudget(perPeriod int64, period time.Duration, clk clock.Clock) *budget {
	return &budget{perPeriod: perPeriod, period: period, clk: clk, tokens: perPeriod, last: clk.Now()}
}

// take spends n if the budget is in credit and reports whether it did.
func (b *budget) take(n int64) bool {
	if b.perPeriod <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Now()
	if since := now.Sub(b.last); since > 0 {
		b.tokens = min(b.perPeriod, b.tokens+int64(float64(b.perPeriod)*(float64(since)/float64(b.period))))
		b.last = now
	}
	if b.tokens < 0 {
		return false
	}
	b.tokens -= n
	return true
}

// --- rate limiter ------------------------------------------------------------------

type limiter struct {
	rate   int64
	clk    clock.Clock
	mu     sync.Mutex
	tokens int64
	last   time.Time
}

func newLimiter(rate int64, clk clock.Clock) *limiter {
	return &limiter{rate: rate, clk: clk, tokens: rate, last: clk.Now()}
}

// wait blocks until n bytes may be moved.
func (l *limiter) wait(ctx context.Context, n int64) {
	if l.rate <= 0 {
		return
	}
	for {
		l.mu.Lock()
		now := l.clk.Now()
		l.tokens = min(l.rate, l.tokens+int64(now.Sub(l.last).Seconds()*float64(l.rate)))
		l.last = now
		if l.tokens >= n || n > l.rate {
			l.tokens -= n
			l.mu.Unlock()
			return
		}
		need := time.Duration(float64(n-l.tokens) / float64(l.rate) * float64(time.Second))
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-l.clk.After(need):
		}
	}
}
