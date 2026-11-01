// Package repair restores redundancy and reclaims space (docs/06): the leader-only GC
// worker and repair scanner, the scrub sampler, and the per-node orphan sweep.
package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	ScrubPeriod  time.Duration // the background passes verify every chunk once per ScrubPeriod (0 = none; docs/06 §5, I-215)
	S3ScrubBytes int64         // bucket bytes the scrub may fetch per day to verify objects (0 = HEAD only)
	RateBytes    int64         // shard bytes per second moved by repair (0 = unlimited)
	Grace        time.Duration // GC grace
	GCInterval   time.Duration
	ScanInterval time.Duration
	ReportTTL    time.Duration // reported bad slots expire after this
	Retention    time.Duration // the trash's retention (docs/06 §2, trash.retention); 0 = never purged on its own
	// Home placement (docs/14 §7, I-295): slots away from their home for placement only
	// move while the gate is open. It opens at the end of a pass whose movable bytes
	// reach HomeStart (a fraction) of the stored bytes, or by OpenHome, and closes when a
	// pass finds none movable; an open pass moves at most HomePerPass chunks: HomeBatch
	// when set, else HomePerWorker for each of Workers, so raising the fan-out for a
	// refill hurries a rebalance too. HomeStart 0 never opens it on its own.
	HomeStart float64
	HomeBatch int // 0: HomePerWorker × Workers
	// Workers is how many chunks a pass repairs at once (docs/06 §4, I-174): each chunk's
	// steps stay in sequence, chunks overlap; at most MaxWorkers, below 1 means 1.
	Workers int
	// SumsBatch is how many chunks without recorded data-shard sums a pass fetches,
	// verifies and records sums for (docs/06 §4, I-201); 0 disables the backfill.
	SumsBatch int
	// SweepInterval is how often a pass visits every chunk (docs/06 §4a, I-379): the scrub,
	// the home and sums queues and the away counts come from sweeps; the passes between
	// visit only the chunks that need work. 0 sweeps every pass.
	SweepInterval time.Duration
}

// DefaultScrubPeriod is how long the background scrub takes to verify the whole volume
// (`repair.scrub_period`): a week, about stored bytes ÷ 7 days of reads and traffic.
const DefaultScrubPeriod = 7 * 24 * time.Hour

// DefaultRetention is how long a deleted entry waits in the trash (docs/12 §10).
const DefaultRetention = 30 * 24 * time.Hour

// DefaultHomeStart is the share of the stored bytes that must be movable away from home
// before placement-only moves start on their own (`repair.home_start`, docs/14 §7).
const DefaultHomeStart = 0.05

// HomeHold is how long the home gate stays shut after a pass whose home moves all failed
// (docs/14 §7, I-311): the moves resume once it passes and the threshold still holds.
const HomeHold = 30 * time.Minute

// FullHold is how long a target that answered ErrFull to a repair PUT is no destination
// for a refill or a home move (I-419): the flusher's hold-down for the same answer
// (docs/14 §7).
const FullHold = 10 * time.Minute

// HomePerWorker is how many home moves a pass makes for each of Config.Workers while the
// gate is open and Config.HomeBatch is 0 (docs/14 §7, I-295): 64 at the default fan-out.
const HomePerWorker = 16

// HomePerPass is the home moves one pass makes while the gate is open: HomeBatch when
// set, else HomePerWorker for each worker (a fan-out below 1 counts as 1).
func (c Config) HomePerPass() int {
	if c.HomeBatch > 0 {
		return c.HomeBatch
	}
	return HomePerWorker * max(c.Workers, 1)
}

// MaxWorkers bounds Config.Workers (`repair.workers`).
const MaxWorkers = 64

// DefaultSweepInterval is how often the leader's pass visits every chunk (I-379).
const DefaultSweepInterval = time.Hour

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		FillAfter: 10 * time.Minute, ScrubPeriod: DefaultScrubPeriod, S3ScrubBytes: 1 << 30, RateBytes: 50 << 20, Grace: 10 * time.Minute,
		GCInterval: 10 * time.Minute, ScanInterval: time.Minute, ReportTTL: 10 * time.Minute, Retention: DefaultRetention,
		HomeStart: DefaultHomeStart, Workers: 4, SumsBatch: 256, SweepInterval: DefaultSweepInterval,
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
	ScrubBad     int   `json:"scrub_bad"` // slots found corrupt by scrub
	Homed        int   `json:"homed"`     // chunks whose away slots moved home (docs/14 §7)
	BytesMoved   int64 `json:"bytes_moved"`
	Errors       int   `json:"errors"`
	NoRoom       int   `json:"no_room"` // chunks with a bad slot and no target with room for the refill: not read (I-419)
	// the data-shard sums backfill (docs/06 §4, I-201): chunks whose sums this pass
	// recorded, and chunks still without them after it
	SumsRecorded int `json:"sums_recorded"`
	SumsPending  int `json:"sums_pending"`
	// home placement (docs/14 §7): the referenced chunks' shard bytes, and the slots of
	// healthy chunks away from their home, their bytes and the bytes a move could take
	// home now; the gate compares MovableBytes with HomeStart × StoredBytes (I-295)
	StoredBytes  int64 `json:"stored_bytes"`
	AwaySlots    int   `json:"away_slots"`
	AwayBytes    int64 `json:"away_bytes"`
	MovableBytes int64 `json:"movable_bytes"`
	HomeOpen     bool  `json:"home_open"` // the gate after this pass: the next one moves placement-only slots
	DiskSkew     int   `json:"disk_skew"` // away slots whose home is another disk of their host (docs/14 §7): the last sweep's
	Swept        bool  `json:"swept"`     // this pass visited every chunk (I-379)
	badChunks    int   // chunks with a bad slot or a pending migration: home moves wait for a pass without
	homeFailed   int   // home moves this pass tried that committed nothing (I-311)
	homeWaiting  bool  // a movable chunk this pass did not move: the sweep queues it
	retry        int   // chunks whose repair tried and failed: their class is retried, not stuck (I-380)
}

// HomeStatus is the home gate as `status` reports it (`repair.home`, docs/12): the last
// completed pass's counts and whether placement-only moves run.
type HomeStatus struct {
	AwaySlots    int     `json:"away_slots"`
	AwayBytes    int64   `json:"away_bytes"`
	MovableBytes int64   `json:"movable_bytes"`
	StoredBytes  int64   `json:"stored_bytes"`
	Start        float64 `json:"start"` // repair.home_start
	Open         bool    `json:"open"`
	Forced       bool    `json:"forced,omitempty"` // opened by `volume rebalance`
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
	s.Homed += o.Homed
	s.NoRoom += o.NoRoom
	s.StoredBytes += o.StoredBytes
	s.AwaySlots += o.AwaySlots
	s.AwayBytes += o.AwayBytes
	s.MovableBytes += o.MovableBytes
	s.DiskSkew += o.DiskSkew
	s.retry += o.retry
	s.badChunks += o.badChunks
	s.homeFailed += o.homeFailed
	s.BytesMoved += o.BytesMoved
	s.Errors += o.Errors
	s.SumsRecorded += o.SumsRecorded
}

// Repairer is the leader's scanner.
type Repairer struct {
	// MinCodec, when set, is the oldest codec a live member reads (the prober's): no
	// sums are backfilled while it is below codec 5, so a pass fetches no chunk only to
	// have its SetShardSums refused (I-219). nil backfills.
	MinCodec func() uint8
	// Held, when set, reports a node whose host is held slow (docs/05 §6, I-371): no refill
	// or home move goes to it, while its slots stay good. nil holds nothing.
	Held func(meta.NodeIdx) bool
	// Room, when set, is a target's free bytes by its last probe (the prober's): a target
	// with less than a shard's worth is no destination, so a chunk is never read for a
	// refill its only target would refuse (I-419). known is false for a bucket, a device
	// or an unprobed row, which count as having room. nil counts every target as having
	// room.
	Room func(meta.NodeIdx) (free int64, known bool)
	// warnRepair and warnPut keep a fault at one line a minute per site (I-124)
	warnRepair, warnPut logx.Limiter
	sumsLeft            atomic.Int64 // the pass's remaining sums backfills (SumsBatch, I-201)
	homeLeft            atomic.Int64 // the pass's remaining home moves (HomeBatch, I-295)
	st                  *meta.Store
	prop                meta.Proposer
	live                Liveness
	shards              Shards
	rd                  ChunkReader
	clk                 clock.Clock
	cfg                 Config
	log                 *slog.Logger
	codecs              *ec.Pool
	limit               *Limiter
	s3scrub             *budget         // bucket bytes the scrub may fetch (docs/06 §5, I-151)
	Events              *meta.EventRing // chunk history (nil unless dev.enabled)

	scanMu    sync.Mutex // one pass at a time: `check --scrub` never overlaps the background loop
	mu        sync.Mutex
	reported  map[chunk.ShardKey]time.Time // reader bad-slot reports
	last      Stats
	lastAt    time.Time
	prevSweep time.Time // the start of the previous sweep while leading (zero: none, I-215, I-379)
	// what passes between sweeps work from (I-379, I-380)
	stuck       map[string]stuckClass // classes a pass could do nothing for, by key
	homeQueue   []meta.Hash           // the last sweep's movable chunks not yet moved
	sumsQueue   []meta.Hash           // the last sweep's chunks without sums not yet recorded
	sumsPending int                   // chunks without sums: the last sweep's count less what passes recorded since
	swept       Stats                 // the last sweep's statistics: away, movable and disk skew between sweeps
	replaced    bool                  // a pass since the last sweep re-placed chunks: their home is uncounted
	sweptDest   map[meta.NodeIdx]bool // the targets that could take a shard at the last sweep
	pass        int
	progress    Progress // the pass under way, while running
	running     bool
	// the home gate (docs/14 §7, I-295)
	home       *meta.HomeView             // the previous pass's view: a change re-evaluates the gate
	homeOpen   bool                       // placement-only moves run
	homeForced bool                       // opened by OpenHome: stays open until nothing is movable
	homeIdle   bool                       // the previous pass found no bad slot and no migration
	homeHeld   time.Time                  // a pass whose home moves all failed shuts the gate until then (I-311)
	full       map[meta.NodeIdx]time.Time // targets that answered ErrFull, not a destination until then
	noRoomOn   []meta.NodeIdx             // the last pass's live targets without room (I-421)
}

// New wires a repairer. log may be nil.
func New(st *meta.Store, prop meta.Proposer, live Liveness, shards Shards, rd ChunkReader, clk clock.Clock, cfg Config, log *slog.Logger) *Repairer {
	if log == nil {
		log = slog.Default()
	}
	return &Repairer{
		st: st, prop: prop, live: live, shards: shards, rd: rd, clk: clk, cfg: cfg, log: log, codecs: &ec.Pool{},
		limit: NewLimiter(cfg.RateBytes, clk), s3scrub: newBudget(cfg.S3ScrubBytes, 24*time.Hour, clk), reported: map[chunk.ShardKey]time.Time{},
		full: map[meta.NodeIdx]time.Time{},
	}
}

// OpenHome opens the home gate whatever HomeStart says (`volume rebalance`, I-295): the
// following passes move placement-only away slots until a pass finds none movable.
func (r *Repairer) OpenHome() {
	r.mu.Lock()
	r.homeOpen, r.homeForced, r.homeHeld = true, true, time.Time{}
	r.mu.Unlock()
}

// Home returns the gate and the last completed pass's counts.
func (r *Repairer) Home() HomeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return HomeStatus{
		AwaySlots: r.last.AwaySlots, AwayBytes: r.last.AwayBytes, MovableBytes: r.last.MovableBytes, StoredBytes: r.last.StoredBytes,
		Start: r.cfg.HomeStart, Open: r.homeOpen, Forced: r.homeForced,
	}
}

// HomeFlow is the shard bytes home moves would take from one target to another.
type HomeFlow struct {
	From     meta.NodeIdx `json:"from"`
	FromName string       `json:"from_name"`
	To       meta.NodeIdx `json:"to"`
	ToName   string       `json:"to_name"`
	Slots    int          `json:"slots"`
	Bytes    int64        `json:"bytes"`
}

// HomePlan is `volume rebalance --dry-run` (docs/12): the moves home placement would make
// now, per pair of targets, and the away slots whose home cannot take them (L-220).
type HomePlan struct {
	Flows        []HomeFlow `json:"flows"`
	AwaySlots    int        `json:"away_slots"`
	AwayBytes    int64      `json:"away_bytes"`
	MovableSlots int        `json:"movable_slots"`
	MovableBytes int64      `json:"movable_bytes"`
	StoredBytes  int64      `json:"stored_bytes"`
	Gate         HomeStatus `json:"gate"`
}

// PlanHome walks the chunk table and lists what home moves would do now, moving nothing.
// Chunks awaiting a migration are left out: the migration places them (docs/14 §7).
func (r *Repairer) PlanHome() HomePlan {
	all := r.st.Nodes()
	vol := r.st.Volume()
	names := map[meta.NodeIdx]string{}
	for i := range all {
		names[all[i].Idx] = all[i].Name
	}
	hv := meta.NewHomeView(all)
	type pair struct{ from, to meta.NodeIdx }
	flows := map[pair]*HomeFlow{}
	p := HomePlan{Gate: r.Home()}
	r.st.ForEachChunk(func(c meta.Chunk) bool {
		if c.Refs == 0 {
			return true
		}
		shard := int64(ec.ShardLen(int(c.Size), int(c.K)))
		for _, n := range c.Placement {
			if n != 0 {
				p.StoredBytes += shard
			}
		}
		if k, m := meta.TargetPolicy(c, vol); k != c.K || m != c.M {
			return true
		}
		away := hv.Away(c.Placement, int(c.K), hv.Home(c.Hash, int(c.K), int(c.M)))
		p.AwaySlots += len(away)
		p.AwayBytes += int64(len(away)) * shard
		for _, mv := range hv.Moves(c, func(n meta.NodeIdx) bool { return r.canTake(n, need(c, c.K)) }) {
			f := flows[pair{mv.From, mv.To}]
			if f == nil {
				f = &HomeFlow{From: mv.From, FromName: names[mv.From], To: mv.To, ToName: names[mv.To]}
				flows[pair{mv.From, mv.To}] = f
			}
			f.Slots++
			f.Bytes += shard
			p.MovableSlots++
			p.MovableBytes += shard
		}
		return true
	})
	for _, f := range flows {
		p.Flows = append(p.Flows, *f)
	}
	sort.Slice(p.Flows, func(i, j int) bool {
		if p.Flows[i].Bytes != p.Flows[j].Bytes {
			return p.Flows[i].Bytes > p.Flows[j].Bytes
		}
		if p.Flows[i].From != p.Flows[j].From {
			return p.Flows[i].From < p.Flows[j].From
		}
		return p.Flows[i].To < p.Flows[j].To
	})
	return p
}

// writable is the set a refill of need bytes may place on: live (not suspect), not held
// slow (I-371), not holding back an ErrFull answer and with room for the shard by its
// last probe (I-419). blocked counts the live targets left out for the last two reasons.
func (r *Repairer) writable(need int64) (live map[meta.NodeIdx]bool, blocked int) {
	live = meta.LiveSet(r.live.Live())
	for n := range live {
		switch {
		case r.Held != nil && r.Held(n):
			delete(live, n)
		case !r.canTake(n, need):
			delete(live, n)
			blocked++
		}
	}
	return live, blocked
}

// need is what a refill of one shard of c under k takes on a target: the shard and the
// store's header, as the store reserves it.
func need(c meta.Chunk, k uint8) int64 {
	return int64(chunk.HeaderSize + ec.ShardLen(int(c.Size), int(k)))
}

// volNeed is need for a full chunk of the volume.
func volNeed(vol meta.Volume) int64 {
	return int64(chunk.HeaderSize + ec.ShardLen(int(vol.ChunkSize), int(vol.K)))
}

// hasRoom reports whether n's last probe left room for need bytes; unknown is room.
func (r *Repairer) hasRoom(n meta.NodeIdx, need int64) bool {
	if r.Room == nil {
		return true
	}
	free, known := r.Room(n)
	return !known || free >= need
}

// canTake reports whether n can take a shard of need bytes moved or refilled: dest and
// room (I-419).
func (r *Repairer) canTake(n meta.NodeIdx, need int64) bool {
	return r.dest(n) && r.hasRoom(n, need)
}

// NoRoomTargets lists the live targets the last pass could not refill onto: marked full
// by an ErrFull answer or without room by their last probe (I-421).
func (r *Repairer) NoRoomTargets() []meta.NodeIdx {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.noRoomOn)
}

// dest reports whether n can take a shard moved or refilled home: live (not suspect),
// not held slow (I-371) and not holding back an ErrFull answer.
func (r *Repairer) dest(n meta.NodeIdx) bool {
	if !r.live.IsLive(n) || (r.Held != nil && r.Held(n)) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.full[n]
	if ok && r.clk.Now().After(until) {
		delete(r.full, n)
		return true
	}
	return !ok
}

func (r *Repairer) markFull(n meta.NodeIdx) {
	r.mu.Lock()
	r.full[n] = r.clk.Now().Add(FullHold)
	r.mu.Unlock()
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
		} else {
			r.LostLeadership()
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
func (r *Repairer) Scan(ctx context.Context) (Stats, error) { return r.scan(ctx, false) }

// Scrub is one pass that verifies every chunk (`check --scrub`), with the configured
// hold-downs and rate limit, serialized with the background passes.
func (r *Repairer) Scrub(ctx context.Context) (Stats, error) { return r.scan(ctx, true) }

// LostLeadership forgets the previous pass, so the next pass as leader scrubs the
// ScanInterval before it rather than everything since this node last led (I-215).
func (r *Repairer) LostLeadership() {
	r.mu.Lock()
	r.prevSweep = time.Time{}
	r.homeQueue, r.sumsQueue, r.stuck, r.sweptDest = nil, nil, nil, nil
	r.homeOpen, r.homeForced, r.home = false, false, nil // a new leader judges the gate afresh
	r.mu.Unlock()
}

// ScrubDue reports whether a pass covering (from, to] scrubs the chunk h under a scrub
// period of p (docs/06 §5, I-215): its phase, the first eight bytes of the hash mod p,
// lies in the arc (from mod p, to mod p]. An arc of p or longer covers every chunk; p = 0
// covers none. The phase is spread evenly by the hash, so each pass verifies about
// (to − from) ÷ p of the volume and every chunk comes round once per period.
func ScrubDue(h meta.Hash, from, to time.Time, p time.Duration) bool {
	if p <= 0 || !to.After(from) {
		return false
	}
	if to.Sub(from) >= p {
		return true
	}
	period := uint64(p)
	phase := binary.BigEndian.Uint64(h[:8]) % period
	a := uint64(from.UnixNano()) % period //nolint:gosec // a wall-clock time after 1970
	b := uint64(to.UnixNano()) % period   //nolint:gosec // a wall-clock time after 1970
	if a < b {
		return a < phase && phase <= b
	}
	return phase > a || phase <= b // the arc wraps past p
}

func (r *Repairer) scan(ctx context.Context, scrubAll bool) (Stats, error) {
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
	hv := meta.NewHomeView(all)
	batch := r.cfg.SumsBatch
	if r.MinCodec != nil && r.MinCodec() < meta.CodecShardSums {
		batch = 0 // a member could not read SetShardSums: the leader would refuse it (I-219)
	}
	now := r.clk.Now()
	// the classes that need work now, judged once each (I-378); a stuck one waits for its
	// view to change (I-380)
	view := r.view(all, vol)
	badClasses, badChunks := r.badClasses(vol, nodes, view, now)
	dest := map[meta.NodeIdx]bool{}
	var noRoomOn []meta.NodeIdx
	for i := range all {
		n := &all[i]
		switch {
		case r.canTake(n.Idx, volNeed(vol)):
			dest[n.Idx] = true
		case n.Kind.Target() && n.State == meta.Active && r.live.IsLive(n.Idx) && (r.Held == nil || !r.Held(n.Idx)):
			noRoomOn = append(noRoomOn, n.Idx) // live, and yet no refill can go there (I-421)
		}
	}
	r.mu.Lock()
	r.pass++
	pass := r.pass
	homesMoved := r.home != nil && !r.home.Same(hv)
	if homesMoved && !r.homeForced {
		r.homeOpen = false // the homes changed: the end of this pass re-evaluates the gate (I-295)
	}
	r.home = hv
	// The sweep visits every chunk (I-379): the first pass as leader, every SweepInterval,
	// whenever the homes moved, `check --scrub`, while the gate is open with nothing left
	// in its queue, and once repairs re-placed chunks and no class needs work any more (a
	// refill can leave a slot away from home, which only a sweep counts). Any other pass
	// visits only where work is.
	// a target that can take shards again (a host back from the dead, a full disk with
	// room) may make movable what the last sweep counted away and could not move (I-379)
	destGained := false
	for n := range dest {
		destGained = destGained || !r.sweptDest[n]
	}
	sweep := scrubAll || r.prevSweep.IsZero() || now.Sub(r.prevSweep) >= r.cfg.SweepInterval || homesMoved ||
		(r.homeOpen && len(r.homeQueue) == 0) || (r.replaced && len(badClasses) == 0) ||
		(destGained && r.swept.AwayBytes > r.swept.MovableBytes)
	from := r.prevSweep // the scrub's arc: since the previous sweep, or one SweepInterval on the first (I-215)
	if from.IsZero() {
		from = now.Add(-max(r.cfg.SweepInterval, r.cfg.ScanInterval))
	}
	if sweep {
		r.prevSweep, r.sweptDest = now, dest
	}
	scrub := func(h meta.Hash) bool {
		return scrubAll || (sweep && ScrubDue(h, from, now, r.cfg.ScrubPeriod))
	}
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
	homeBatch := 0
	if r.homeOpen && r.homeIdle {
		homeBatch = r.cfg.HomePerPass()
	}
	r.mu.Unlock()
	r.homeLeft.Store(int64(homeBatch))
	r.sumsLeft.Store(int64(batch))

	// policy migration order: fewest effective parity first
	var work []meta.Chunk
	if sweep {
		r.st.ForEachChunk(func(c meta.Chunk) bool {
			if c.Refs > 0 {
				work = append(work, c)
			}
			return true
		})
	} else {
		work = r.targeted(badClasses, reportedKeys, homeBatch, batch)
	}
	sort.SliceStable(work, func(i, j int) bool { return work[i].M < work[j].M })

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
		wg       sync.WaitGroup
		smu      sync.Mutex
		sem      = make(chan struct{}, workers)
		progress = map[string]int{} // class → chunks of it this pass changed, or tried to and failed
		visited  = map[string]bool{}
		homeQ    []meta.Hash // a sweep's movable chunks left for later passes
		sumsQ    []meta.Hash // a sweep's chunks without sums left for later passes
		noSums   int
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
			// the policy is read per chunk, never once per pass: a SetVolume applied
			// mid-pass retargets every chunk started after it (docs/06 §7.5, I-417); the
			// classes the pass chose keep the view it was judged with
			one := r.scanOne(ctx, c, r.st.Volume(), nodes, hv, reportedKeys[c.Hash], scrub(c.Hash))
			r.mu.Lock()
			r.progress.InFlight--
			r.progress.Scanned++
			r.progress.BytesMoved += one.BytesMoved
			r.mu.Unlock()
			key := meta.ClassKey(&c, vol.ChunkSize)
			smu.Lock()
			st.add(one)
			visited[key] = true
			// a failed attempt (a PUT, a read, a migration short of k acks) is retried next
			// pass; only a class nothing could be tried for is stuck (I-380)
			progress[key] += one.Repaired + one.Partial + one.Migrated + one.Homed + one.Errors + one.Deferred + one.retry
			if sweep && one.homeWaiting {
				homeQ = append(homeQ, c.Hash)
			}
			if sweep && c.Sums == nil {
				noSums++
				if one.SumsRecorded == 0 {
					sumsQ = append(sumsQ, c.Hash)
				}
			}
			smu.Unlock()
		}(c)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return st, ctx.Err()
	}
	st.Swept = sweep
	st.StoredBytes = int64(r.st.Stats().StoredBytes) //nolint:gosec // bytes of a volume
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noRoomOn = nil
	if st.NoRoom > 0 {
		r.noRoomOn = noRoomOn
	}
	for key := range badClasses {
		if !visited[key] {
			continue
		}
		if progress[key] == 0 {
			r.stuck[key] = stuckClass{view: view, at: now} // nothing it could do: wait for the view to change (I-380)
		} else {
			delete(r.stuck, key)
		}
	}
	if sweep {
		sortHashes(homeQ)
		sortHashes(sumsQ)
		r.homeQueue, r.sumsQueue = capQueue(homeQ), capQueue(sumsQ)
		r.sumsPending = max(noSums-st.SumsRecorded, 0)
		r.swept = st // what only a sweep counts: away, movable, disk skew
		for key := range r.stuck {
			if !badClasses[key] {
				delete(r.stuck, key) // healthy again
			}
		}
	} else {
		r.sumsPending = max(r.sumsPending-st.SumsRecorded, 0)
		st.AwaySlots, st.AwayBytes, st.MovableBytes, st.DiskSkew = r.swept.AwaySlots, r.swept.AwayBytes, r.swept.MovableBytes, r.swept.DiskSkew
	}
	st.SumsPending = r.sumsPending
	r.replaced = st.Repaired+st.Partial+st.Migrated > 0 || (r.replaced && !sweep)
	r.last, r.lastAt = st, r.clk.Now()
	r.homeIdle = st.badChunks == 0 && badChunks == 0
	switch {
	case st.MovableBytes == 0:
		r.homeOpen, r.homeForced = false, false
	case st.Homed == 0 && st.homeFailed > 0:
		// every move this pass tried failed: retrying each pass would only move the same
		// bytes again for nothing, so the gate shuts for HomeHold (I-311)
		r.homeOpen, r.homeForced = false, false
		r.homeHeld = r.clk.Now().Add(HomeHold)
	case !r.homeOpen && r.cfg.HomeStart > 0 && !r.clk.Now().Before(r.homeHeld) && float64(st.MovableBytes) >= r.cfg.HomeStart*float64(st.StoredBytes):
		r.homeOpen = true
	}
	st.HomeOpen = r.homeOpen
	r.last.HomeOpen = r.homeOpen
	return st, nil
}

// stuckClass is a class whose chunks a pass visited without changing or trying to change
// any of them (no live spare to place on, fewer than k readable slots), and the view it
// was judged under (I-380).
type stuckClass struct {
	view string
	at   time.Time
}

// StuckRetry is how long a stuck class waits before a pass visits it again under an
// unchanged view (I-380).
const StuckRetry = 10 * time.Minute

// QueueMax bounds the home and sums queues a sweep leaves for later passes; what does not
// fit waits for the next sweep.
const QueueMax = 1 << 20

func capQueue(q []meta.Hash) []meta.Hash {
	if len(q) > QueueMax {
		q = q[:QueueMax]
	}
	return q
}

func sortHashes(q []meta.Hash) {
	slices.SortFunc(q, func(a, b meta.Hash) int { return bytes.Compare(a[:], b[:]) })
}

// view is everything a class's judgement and its repair's destinations read besides the
// class: each member's liveness, state, hold and full mark, and the volume's policy. A
// stuck class is visited again once it changes (I-380).
func (r *Repairer) view(all []meta.Node, vol meta.Volume) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d+%d;", vol.K, vol.M)
	for i := range all {
		n := all[i].Idx
		fmt.Fprintf(&b, "%d:%d:%t:%t:%t;", n, all[i].State, r.live.IsLive(n), r.dest(n), r.hasRoom(n, volNeed(vol)))
	}
	return b.String()
}

// badClasses judges every placement class once (I-378) and returns those with a bad slot
// or a pending migration that are not stuck under this view, and how many chunks are in
// a class with a bad slot or a pending migration at all.
func (r *Repairer) badClasses(vol meta.Volume, nodes map[meta.NodeIdx]meta.Node, view string, now time.Time) (map[string]bool, int) {
	out := map[string]bool{}
	bad := 0
	in := meta.HealthInput{Nodes: nodes, Live: r.live, FillAfter: r.cfg.FillAfter}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stuck == nil {
		r.stuck = map[string]stuckClass{}
	}
	for _, cl := range r.st.Classes() {
		h := meta.ChunkState(cl.Representative(vol), vol, in)
		if !h.RepairPending() && !h.PolicyStale {
			continue
		}
		bad += cl.Chunks
		if s, ok := r.stuck[cl.Key]; ok && s.view == view && now.Sub(s.at) < StuckRetry {
			continue
		}
		out[cl.Key] = true
	}
	return out, bad
}

// targeted is a pass's work outside a sweep (I-379): the chunks of the classes that need
// work, the chunks with a reported slot, and the next chunks of the home queue (while the
// gate is open) and of the sums queue.
func (r *Repairer) targeted(classes map[string]bool, reported map[meta.Hash]map[int]bool, homeBatch, sumsBatch int) []meta.Chunk {
	seen := map[meta.Hash]bool{}
	var work []meta.Chunk
	add := func(h meta.Hash) {
		if seen[h] {
			return
		}
		seen[h] = true
		if c, ok := r.st.GetChunk(h); ok && c.Refs > 0 {
			work = append(work, c)
		}
	}
	keys := make([]string, 0, len(classes))
	for key := range classes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, h := range r.st.ClassMembers(key) {
			add(h)
		}
	}
	hashes := make([]meta.Hash, 0, len(reported))
	for h := range reported {
		hashes = append(hashes, h)
	}
	sortHashes(hashes)
	for _, h := range hashes {
		add(h)
	}
	r.mu.Lock()
	n := min(homeBatch, len(r.homeQueue))
	home := r.homeQueue[:n]
	r.homeQueue = r.homeQueue[n:]
	m := min(sumsBatch, len(r.sumsQueue))
	sums := r.sumsQueue[:m]
	r.sumsQueue = r.sumsQueue[m:]
	r.mu.Unlock()
	for _, h := range home {
		add(h)
	}
	for _, h := range sums {
		add(h)
	}
	return work
}

// scanOne classifies one chunk and repairs, migrates or scrubs it (docs/06 §4 cases
// 1–6), returning what it did as that chunk's share of the pass statistics.
func (r *Repairer) scanOne(ctx context.Context, c meta.Chunk, vol meta.Volume, nodes map[meta.NodeIdx]meta.Node, hv *meta.HomeView, reportedIdx map[int]bool, scrubDue bool) (st Stats) {
	st.Scanned++
	shard := int64(ec.ShardLen(int(c.Size), int(c.K)))
	for _, n := range c.Placement {
		if n != 0 {
			st.StoredBytes += shard
		}
	}
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
		if r.homeOne(ctx, c, vol, hv, shard, &st) {
			return st
		}
		if c.Sums == nil && r.sumsLeft.Add(-1) >= 0 { // a healthy chunk without sums: record them (I-201)
			if err := r.backfillSums(ctx, c); err != nil {
				st.Errors++
				r.warnRepair.Warn(r.log, "shard sums not recorded", "chunk", fmt.Sprintf("%x", c.Hash[:8]), "err", err)
			} else {
				st.SumsRecorded++
			}
		}
		if !scrubDue {
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
	st.badChunks++
	if readable < int(c.K) {
		st.Unrepairable++
		r.Events.Record(c.Hash, meta.EvUnrepairable, fmt.Sprintf("%d readable slots < k=%d", readable, c.K))
		return st
	}
	outcome, err := r.repairOne(ctx, c, bad, kept, migrate, vol, hv)
	if err != nil {
		st.Errors++
		r.warnRepair.Warn(r.log, "repair failed", "chunk", fmt.Sprintf("%x", c.Hash[:8]), "err", err)
		return st
	}
	st.BytesMoved += outcome.bytes
	if outcome.tried && !outcome.changed {
		st.retry++
	}
	switch {
	case !outcome.changed && migrate:
		st.Deferred++ // fewer than k acks: the old encoding stays committed, retried next pass
	case !outcome.changed:
		// no live spare could take the bad slots; counted as still degraded
		if outcome.noRoom {
			st.NoRoom++
		}
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

// backfillSums fetches, decodes and verifies a chunk through the reader, re-encodes it
// and records the data shards' hashes with SetShardSums (docs/06 §4, I-201): what the
// flusher would have recorded had every member read codec 5 when the chunk was written.
func (r *Repairer) backfillSums(ctx context.Context, c meta.Chunk) error {
	data, err := r.rd.ReadChunk(ctx, c.Hash) // hash-verified, hedged across holders
	if err != nil {
		return err
	}
	codec, err := r.codecs.Get(c.K, c.M)
	if err != nil {
		return err
	}
	shards, err := codec.Encode(data)
	if err != nil {
		return err
	}
	sums := make([]meta.Hash, int(c.K))
	for j := range sums {
		sums[j] = sha256.Sum256(shards[j])
	}
	res, _, err := r.prop.Propose(ctx, meta.Command{Op: meta.OpSetShardSums, SetShardSums: &meta.SetShardSums{Hash: c.Hash, Sums: sums}})
	if err != nil {
		return err
	}
	return res.Err
}

// homeOne counts c's away slots and, while the gate lets this pass move one more chunk,
// moves the movable ones home through repairOne (docs/14 §7, I-294, I-295). It reports
// whether it tried; a chunk it did not try goes on to the backfill and the scrub.
func (r *Repairer) homeOne(ctx context.Context, c meta.Chunk, vol meta.Volume, hv *meta.HomeView, shard int64, st *Stats) bool {
	home := hv.Home(c.Hash, int(c.K), int(c.M))
	away := hv.Away(c.Placement, int(c.K), home)
	if len(away) == 0 {
		return false
	}
	st.AwaySlots += len(away)
	st.AwayBytes += int64(len(away)) * shard
	st.DiskSkew += diskSkew(hv, c, home, away)
	movable := hv.Movable(c, func(n meta.NodeIdx) bool { return r.canTake(n, need(c, c.K)) })
	st.MovableBytes += int64(len(movable)) * shard
	if len(movable) == 0 {
		return false
	}
	if r.homeLeft.Add(-1) < 0 {
		st.homeWaiting = true
		return false
	}
	issues := make([]meta.SlotIssue, 0, len(away))
	kept := slices.Clone(c.Placement)
	for _, i := range away {
		issues = append(issues, meta.SlotIssue{Idx: i, Reason: meta.ReasonAway, Readable: true})
		kept[i] = 0
	}
	out, err := r.repairOne(ctx, c, issues, kept, false, vol, hv)
	st.BytesMoved += out.bytes
	if err != nil {
		st.Errors++
		st.homeFailed++
		r.warnRepair.Warn(r.log, "home move failed", "chunk", fmt.Sprintf("%x", c.Hash[:8]), "err", err)
		return true
	}
	if !out.changed {
		st.homeFailed++ // every PUT home failed: nothing placeable improved
		return true
	}
	st.Homed++
	r.Events.Record(c.Hash, meta.EvHomed, fmt.Sprintf("slots %v moved home", movable))
	return true
}

// diskSkew counts c's away slots whose home is a free target on the slot's own host: what
// home placement moves between the disks of one host (docs/14 §7, I-172).
func diskSkew(hv *meta.HomeView, c meta.Chunk, home []meta.NodeIdx, away []int) int {
	k := int(c.K)
	n := 0
	for _, i := range away {
		lo, hi := 0, min(k, len(home))
		if i >= k {
			lo, hi = hi, len(home)
		}
		for _, t := range home[lo:hi] {
			if t != 0 && hv.Domain(t) == hv.Domain(c.Placement[i]) && !slices.Contains(c.Placement, t) {
				n++
				break
			}
		}
	}
	return n
}

type outcome struct {
	changed bool // a new placement was committed
	tried   bool // a PUT was attempted or the commit lost a race: the next pass retries (I-380)
	full    bool
	noRoom  bool // nothing to put because every target left was full or without room (I-419)
	bytes   int64
}

// repairOne re-places the bad slots (or every slot for a migration), commits the new
// placement and only then deletes superseded shards (docs/06 §4 steps 1–6).
func (r *Repairer) repairOne(ctx context.Context, c meta.Chunk, bad []meta.SlotIssue, kept []meta.NodeIdx, migrate bool, vol meta.Volume, hv *meta.HomeView) (outcome, error) {
	k, m := c.K, c.M
	if migrate {
		k, m = meta.TargetPolicy(c, vol)
	}
	codec, err := r.codecs.Get(k, m)
	if err != nil {
		return outcome{}, err
	}
	// the plan comes before the read: a chunk with nowhere to put a shard is not fetched
	// only to have its PUT refused (I-419)
	live, blocked := r.writable(need(c, k))
	plan := planSlots(c, bad, kept, migrate, k, m, hv, r.st.Nodes(), live, func(n meta.NodeIdx) bool { return live[n] })
	newPlace, unchanged, readableBad := plan.place, plan.unchanged, plan.readableBad
	if !meta.ValidPlacement(k, m, newPlace) {
		// a planner bug, never a state the FSM accepts: refuse it here, before any PUT
		// moves bytes for a SetPlacement that can only be rejected (I-311)
		return outcome{}, fmt.Errorf("planned placement %v of %d+%d: %w", newPlace, k, m, meta.ErrBadCommand)
	}
	toPut := false
	for i, n := range newPlace {
		if n != 0 && !unchanged[i] {
			toPut = true
		}
	}
	if !toPut {
		return outcome{noRoom: blocked > 0}, nil // nothing placeable: nothing read, nothing tried (L-185, L-223)
	}
	data, err := r.rd.ReadChunk(ctx, c.Hash)
	if err != nil {
		return outcome{}, fmt.Errorf("read: %w", err)
	}
	shards, err := codec.Encode(data)
	if err != nil {
		return outcome{}, err
	}
	var moved int64
	acked := 0
	tried := false
	var revert []int // still-served slots whose PUT failed
	for i, n := range newPlace {
		if n == 0 {
			continue
		}
		if unchanged[i] {
			acked++
			continue
		}
		r.limit.Wait(ctx, int64(len(shards[i])))
		tried = true
		if err := r.shards.Put(ctx, n, chunk.Key(c.Hash, k, m, i), shards[i]); err != nil {
			r.warnPut.Warn(r.log, "repair put failed", "node", n, "slot", i, "err", err)
			if errors.Is(err, chunk.ErrFull) {
				r.markFull(n)
			}
			newPlace[i] = 0
			if readableBad[i] {
				revert = append(revert, i)
			}
			continue
		}
		moved += int64(len(shards[i]))
		acked++
	}
	acked += keepServed(c.Placement, newPlace, unchanged, revert)
	if migrate && acked < int(k) {
		return outcome{bytes: moved, tried: tried}, nil // keep the old encoding committed; try again next pass
	}
	if !migrate {
		stayed := 0
		for _, u := range unchanged {
			if u {
				stayed++
			}
		}
		if acked == stayed {
			return outcome{bytes: moved, tried: tried}, nil // nothing placeable improved; nothing to commit (L-185)
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
		return outcome{bytes: moved, tried: true}, nil
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

// slotPlan is where repairOne puts each slot of a chunk before any PUT: the target per
// slot, the slots that need no PUT, and the slots whose holder still serves them.
type slotPlan struct {
	place       []meta.NodeIdx
	unchanged   []bool
	readableBad map[int]bool
}

// planSlots places the bad slots of c (every slot for a migration) for repairOne. A
// node that still serves a slot of c (kept, or a readable bad slot it stands in for)
// is never given another slot: a slot that keeps its holder (no home could take it, or
// its PUT fails) would otherwise name that node twice, which the FSM refuses (I-311).
func planSlots(c meta.Chunk, bad []meta.SlotIssue, kept []meta.NodeIdx, migrate bool, k, m uint8, hv *meta.HomeView, nodes []meta.Node, live map[meta.NodeIdx]bool, dest func(meta.NodeIdx) bool) slotPlan {
	total := int(k) + int(m)
	newPlace := make([]meta.NodeIdx, total)
	if !migrate {
		copy(newPlace, kept)
	}
	// Every emptied slot goes home first (I-296), and a slot no one serves before one whose
	// holder still serves it (draining, skewed, away): while the readable ones are filled,
	// their holders stand in for them, so a lone spare replaces the missing shard (I-65).
	home := hv.Home(c.Hash, int(k), int(m))
	fill := func() {
		hv.FillHome(newPlace, int(k), home, dest)
		var held []meta.NodeIdx
		for _, n := range newPlace {
			if n != 0 {
				held = append(held, n)
			}
		}
		meta.FillPlacement(c.Hash, newPlace, meta.ExcludeIdx(meta.Candidates(nodes, func(n meta.NodeIdx) bool { return live[n] }), held), int(k), meta.DomainsOf(nodes))
	}
	var standIn []int
	if !migrate {
		for _, b := range bad {
			if b.Readable && newPlace[b.Idx] == 0 {
				newPlace[b.Idx] = c.Placement[b.Idx]
				standIn = append(standIn, b.Idx)
			}
		}
	}
	fill()
	var away []int
	for _, i := range standIn {
		newPlace[i] = 0
	}
	for _, b := range bad {
		if b.Reason == meta.ReasonAway {
			away = append(away, b.Idx)
		}
	}
	if len(away) > 0 {
		// every away slot goes home or nowhere in one step, never onto a bucket, and a
		// slot bound for the holder of one that stays waits with it (I-294, L-220)
		hv.FillAway(newPlace, c.Placement, away, int(k), home, func(n meta.NodeIdx) bool { return !hv.S3(n) && dest(n) })
	}
	fill()
	// A slot whose shard is still served (live draining holder) keeps its node until a
	// replacement is placed: a repair never lowers the readable slot count (I-65, L-184).
	// unchanged marks slots that need no PUT: kept ones and retained ones.
	unchanged := make([]bool, total)
	for i := range unchanged {
		unchanged[i] = !migrate && i < len(kept) && kept[i] != 0
	}
	readableBad := map[int]bool{}
	if !migrate {
		for _, b := range bad {
			if !b.Readable {
				continue
			}
			readableBad[b.Idx] = true
			if newPlace[b.Idx] == 0 || newPlace[b.Idx] == c.Placement[b.Idx] {
				newPlace[b.Idx] = c.Placement[b.Idx]
				unchanged[b.Idx] = true
			}
		}
	}
	// A slot that kept its holder X (no home could take it) while a fill gave X another
	// slot names X twice, which the FSM refuses: the kept slot wins, the other slot is
	// planned again without X (I-311). Each round removes one name, so it ends.
	for {
		i, j := duplicateSlots(newPlace, unchanged)
		if i < 0 {
			break
		}
		newPlace[j] = 0
		if readableBad[j] && !slices.Contains(newPlace, c.Placement[j]) {
			newPlace[j], unchanged[j] = c.Placement[j], true
			continue
		}
		var held []meta.NodeIdx
		for _, n := range newPlace {
			if n != 0 {
				held = append(held, n)
			}
		}
		held = append(held, newPlace[i])
		meta.FillPlacement(c.Hash, newPlace, meta.ExcludeIdx(meta.Candidates(nodes, func(n meta.NodeIdx) bool { return live[n] }), held), int(k), meta.DomainsOf(nodes))
	}
	return slotPlan{place: newPlace, unchanged: unchanged, readableBad: readableBad}
}

// keepServed gives each still-served slot whose PUT failed back to its holder, which
// still serves it, rather than lose a readable slot (I-65), unless a PUT just gave that
// holder another slot of the chunk (I-311). It returns the slots it gave back.
func keepServed(old, place []meta.NodeIdx, unchanged []bool, failed []int) int {
	n := 0
	for _, i := range failed {
		if !slices.Contains(place, old[i]) {
			place[i], unchanged[i] = old[i], true
			n++
		}
	}
	return n
}

// duplicateSlots finds a target named by two slots of place: i is the one to keep (a slot
// that needs no PUT when one does), j the one to plan again; i < 0 when there is none.
func duplicateSlots(place []meta.NodeIdx, unchanged []bool) (int, int) {
	first := map[meta.NodeIdx]int{}
	for j, n := range place {
		if n == 0 {
			continue
		}
		i, seen := first[n]
		if !seen {
			first[n] = j
			continue
		}
		if unchanged[j] && !unchanged[i] {
			return j, i
		}
		return i, j
	}
	return -1, -1
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

// Limiter is a token bucket of bytes a second, shared by repair and the mirror syncs
// (docs/06 §4, docs/16 §3); a rate of 0 or less never waits.
type Limiter struct {
	rate   int64
	clk    clock.Clock
	mu     sync.Mutex
	tokens int64
	last   time.Time
}

// NewLimiter returns a limiter of rate bytes a second.
func NewLimiter(rate int64, clk clock.Clock) *Limiter {
	return &Limiter{rate: rate, clk: clk, tokens: rate, last: clk.Now()}
}

// Wait blocks until n bytes may be moved, or ctx ends.
func (l *Limiter) Wait(ctx context.Context, n int64) {
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
