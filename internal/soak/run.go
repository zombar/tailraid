package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/repair"
)

// Profile sizes a run.
type Profile struct {
	Name          string
	BigFile       int           // bytes of the largest corpus file
	LargeFile     int           // bytes of the large-file phase's one file (0 skips the phase)
	SmallFiles    int           // count of tiny files
	ChurnOps      int           // random-access operations per gateway
	LeaderKills   int           // consensus phase rounds
	FlushIdle     time.Duration // how long to wait for the idle flush after writes
	FillAfter     time.Duration // the targets' repair.fill_after (parity phase waits this)
	Grace         time.Duration // the targets' repair.grace (GC phase waits this)
	SampleEvery   time.Duration // metric sampling period (0 = phase boundaries only)
	ChunkSize     int           // the targets' chunk size (corpus sizes straddle it)
	RestartSettle time.Duration // how long nodes take to come back after a restart
	// PhaseTimeout fails a phase that runs longer and moves on (goroutine dumps are
	// taken first); 0 is no limit. A hard-mounted volume that hangs is the case.
	PhaseTimeout time.Duration
}

// Quick is the compose profile (~10 min).
func Quick() Profile {
	return Profile{
		Name: "quick", BigFile: 3_000_000, LargeFile: 256 << 20, SmallFiles: 150, ChurnOps: 300, LeaderKills: 3, ChunkSize: 256 << 10,
		FlushIdle: 4 * time.Second, FillAfter: 20 * time.Second, Grace: 20 * time.Second, SampleEvery: time.Second, RestartSettle: 15 * time.Second,
		PhaseTimeout: 20 * time.Minute,
	}
}

// Full is the long profile (~1 h): the quick one with a 64 MiB corpus file, a 1 GiB
// large file, more small files and churn, and ten leader kills. `make soak-full` and
// `tailraid verify --full` run it.
func Full() Profile {
	p := Quick()
	p.Name, p.BigFile, p.LargeFile, p.SmallFiles, p.ChurnOps, p.LeaderKills = "full", 64<<20, 1<<30, 2000, 3000, 10
	return p
}

// Record is one line of results.jsonl.
type Record struct {
	T     time.Time         `json:"t"`
	Line  int               `json:"line"` // 1-based line in results.jsonl
	Kind  string            `json:"kind"` // sample | event | check | phase | dump | summary
	Phase string            `json:"phase,omitempty"`
	Name  string            `json:"name,omitempty"`
	Cites string            `json:"cites,omitempty"` // invariants and lifecycle cells a check exercises ("I-12 L-64")
	OK    *bool             `json:"ok,omitempty"`
	Skip  bool              `json:"skipped,omitempty"`
	Text  string            `json:"text,omitempty"`
	File  string            `json:"file,omitempty"`
	Chunk string            `json:"chunk,omitempty"`
	Node  string            `json:"node,omitempty"`
	S     *Sample           `json:"sample,omitempty"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Sample is one metric row (driver counters plus one node's status view).
type Sample struct {
	BytesWritten  int64                      `json:"bytes_written"`
	BytesRead     int64                      `json:"bytes_read"`
	Writes        int64                      `json:"writes"`
	Reads         int64                      `json:"reads"`
	WriteLatency  [len(latencyBuckets)]int64 `json:"write_latency"` // cumulative histogram, see latencyBuckets
	ReadLatency   [len(latencyBuckets)]int64 `json:"read_latency"`
	Degraded      int                        `json:"degraded"`
	RepairPending int                        `json:"repair_pending"`
	Unreadable    int                        `json:"unreadable"`
	StalePolicy   int                        `json:"stale_policy"`
	DomainSkew    int                        `json:"domain_skew"`       // chunks above the host cap (docs/14 §7)
	Chunks        int                        `json:"chunks"`            // live chunks: the most slots one target can hold (I-9)
	Targets       []TargetSample             `json:"targets,omitempty"` // every placement target, for the disk-spread chart
	StoredBytes   uint64                     `json:"stored_bytes"`
	LogicalBytes  uint64                     `json:"logical_bytes"`
	DedupRatio    float64                    `json:"dedup_ratio"`
	Slots         map[string]int             `json:"slots"` // per node name
	LiveNodes     map[string]bool            `json:"live_nodes"`
	Live          int                        `json:"live"`
	FSMAgree      *bool                      `json:"fsm_agree,omitempty"` // nil when not sampled this round
	Health        string                     `json:"health"`
	RepairBacklog int                        `json:"repair_backlog"` // degraded + stale policy
	WALBytes      int64                      `json:"wal_bytes"`      // the sampled node's overlay WAL depth
	DirtyBytes    uint64                     `json:"dirty_bytes"`
	PlanBytes     uint64                     `json:"plan_bytes"`
	FlushBytes    uint64                     `json:"flush_bytes"` // the sampled node's flush buffers (docs/04 §9)
	HeapInuse     uint64                     `json:"heap_inuse"`  // the sampled node's process (docs/12 §8)
	Sys           uint64                     `json:"sys"`
	Goroutines    int                        `json:"goroutines"`
	// the sampled node's machine vitals (docs/12 §8); rates are between this node's
	// consecutive samples, zero when the OS does not expose the counter
	CPUPct       float64 `json:"cpu_pct"`     // the process, percent of one CPU
	MachinePct   float64 `json:"machine_pct"` // every CPU, percent
	Load1        float64 `json:"load1"`
	RSS          uint64  `json:"rss"`
	MemAvailable uint64  `json:"mem_available"`
	MemTotal     uint64  `json:"mem_total"`
	MemPressure  float64 `json:"mem_pressure"`
	DiskFree     uint64  `json:"disk_free"`
	DiskTotal    uint64  `json:"disk_total"`
	DiskWriteBps float64 `json:"disk_write_bps"`
	DiskReadBps  float64 `json:"disk_read_bps"`
}

// vitalsPrev is the last machine report of one node that a rate was computed from, and
// the rates themselves (carried across intervals too short to divide by).
type vitalsPrev struct {
	t                 time.Time
	m                 admin.MachineReport
	cpu, mcpu, wr, rd float64
	hasRates          bool
}

// minRateInterval is the shortest interval a rate is computed over: the samples at
// phase boundaries sit milliseconds from a periodic one, and a counter delta over that
// is noise (as the throughput chart's boundary filter already knows).
const minRateInterval = 250 * time.Millisecond

// latencyBuckets are the fixed upper bounds of the latency histogram in ms (log-spaced,
// the last is +Inf), so histograms from different runs line up.
var latencyBuckets = [14]float64{0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 0}

func bucket(d time.Duration) int {
	ms := float64(d) / float64(time.Millisecond)
	for i := range len(latencyBuckets) - 1 {
		if ms <= latencyBuckets[i] {
			return i
		}
	}
	return len(latencyBuckets) - 1
}

// Run drives one profile against one Env and collects results.
type Run struct {
	// Phases replaces the standard phase table when set (tests inject a phase).
	Phases []Phase

	Env     Env
	Profile Profile
	Log     *slog.Logger

	lastRepair repair.Stats // the last pass repairUntilClean ran, quoted when a phase finds work left
	Out        io.Writer    // results.jsonl (may be nil)
	Rng        *rand.Rand
	Seed       int64    // the Rng's seed, for the report
	Flags      []string // run flags worth recording ("destructive", "restarts")
	Root       string   // volume directory for this run, e.g. /_soak/20261008T1200

	mu        sync.Mutex
	records   []Record
	counters  Sample
	lats      []opLatency
	vitals    map[string]vitalsPrev // per sampled node, for the rates
	repair    repair.Stats          // Σ of every repair and scrub pass the run asked for
	gc        repair.Result         // Σ of every GC pass
	phase     string
	nodes     []NodeRef
	hostOf    map[int]int // disk target → its host (docs/14), learned from status
	failed    int
	started   time.Time
	stopSamp  chan struct{}
	sampDone  chan struct{}
	shared    map[string]any // cross-phase state (the corpus model)
	dumpBytes int            // goroutine dump bytes recorded so far (dumpTotalMax)
}

// opLatency is one measured operation.
type opLatency struct {
	T     time.Time
	Phase string
	Read  bool
	D     time.Duration
}

// Summary is the outcome of a run.
type Summary struct {
	Env       string
	Profile   string
	Seed      int64
	Flags     []string
	Started   time.Time
	Duration  time.Duration
	K, M      uint8
	ChunkSize int
	NodeCount int
	Versions  map[string]string // node name → binary version at the start
	Phases    []PhaseResult
	Failed    int
	Partial   bool   // a phase was skipped for a capability the environment lacks
	Aborted   string // why the run stopped before cleanup ("" for a run that reached it)
	Records   []Record
	Nodes     []NodeRef
	Latency   []LatencyRow
	Totals    Totals
	Coverage  []CoverageRow
}

// PhaseResult aggregates the checks of one phase.
type PhaseResult struct {
	Name    string
	Checks  []Record
	Skipped bool
	Reason  string
	NotRun  bool // the run stopped before this phase (cancelled or aborted)
	Failed  int
	Took    time.Duration
}

// Phase is one scenario; Run.Phases replaces the standard table (tests).
type Phase struct {
	Name string
	Run  func(ctx context.Context, r *Run) error
}

// LatencyRow is the driver-side latency of one operation kind in one phase, over the
// whole phase or only while a fault was in force.
type LatencyRow struct {
	Phase       string
	Kind        string // write | read
	UnderFaults bool
	N           int
	P50         time.Duration
	P99         time.Duration
	Max         time.Duration
}

// Totals are the run's counters.
type Totals struct {
	BytesWritten int64
	BytesRead    int64
	Writes       int64
	Reads        int64
	DedupRatio   float64 // from the last status sample
	MaxWALBytes  int64   // deepest overlay WAL seen in a sample
	MaxFlush     uint64  // most flush buffer bytes seen in a sample
	MaxHeap      uint64  // highest heap in use seen in a sample
	MaxRSS       uint64  // highest resident set seen in a sample (docs/12 §8)
	MaxCPUPct    float64 // the process's busiest sample interval
	MinDiskFree  uint64  // least free space on the sampled node's data disk (0 = not sampled)
	Repair       repair.Stats
	GC           repair.Result
}

// CoverageRow is one invariant or lifecycle cell and the checks that cited it.
type CoverageRow struct {
	ID     string
	Passed int
	Failed int
}

func (r *Run) now() time.Time { return time.Now() }

func (r *Run) emit(rec Record) {
	rec.T = r.now()
	r.mu.Lock()
	if rec.Phase == "" {
		rec.Phase = r.phase
	}
	rec.Line = len(r.records) + 1
	r.records = append(r.records, rec)
	if rec.Kind == "check" && rec.OK != nil && !*rec.OK {
		r.failed++
	}
	if r.Out != nil {
		if rec.S != nil {
			scrubSample(rec.S) // NaN and Inf have no JSON form; a sample is never lost for one field
		}
		b, err := json.Marshal(rec)
		if err != nil { // still: keep the line, say what happened
			b, _ = json.Marshal(Record{T: rec.T, Line: rec.Line, Kind: "event", Phase: rec.Phase, Name: "note", Text: "record could not be encoded: " + err.Error()})
		}
		_, _ = r.Out.Write(append(b, '\n'))
	}
	r.mu.Unlock()
	if r.Log != nil {
		switch rec.Kind {
		case "check":
			state := "ok"
			if rec.OK != nil && !*rec.OK {
				state = "FAIL"
			}
			r.Log.Info("check", "phase", rec.Phase, "name", rec.Name, "result", state, "detail", rec.Text)
		case "event", "phase":
			r.Log.Info(rec.Kind, "phase", rec.Phase, "name", rec.Name, "detail", rec.Text)
		}
	}
}

// event records a timeline marker (kills, restarts, partitions, policy changes).
func (r *Run) event(name, text string) { r.emit(Record{Kind: "event", Name: name, Text: text}) }

// scrubSample replaces NaN and Inf in every float field of s with 0: the sources guard
// their divisions, but a node's own vitals (load, memory pressure) arrive as it sent them.
func scrubSample(s *Sample) {
	for _, f := range []*float64{&s.DedupRatio, &s.CPUPct, &s.MachinePct, &s.Load1, &s.MemPressure, &s.DiskWriteBps, &s.DiskReadBps} {
		if math.IsNaN(*f) || math.IsInf(*f, 0) {
			*f = 0
		}
	}
}

// checkSkipped records a check that could not be made, with the reason: it counts as
// neither pass nor fail, and the report shows SKIPPED rather than a pass that measured
// nothing.
func (r *Run) checkSkipped(cites, name, reason string) {
	r.emit(Record{Kind: "check", Name: name, Cites: cites, Skip: true, Text: reason})
}

// undo collects the restores a phase owes the cluster (start what it killed, heal what it
// cut, activate what it drained, pin the policy back) and runs the ones the phase did not
// get to, on a context that outlives a cancellation, so a failed or interrupted phase does
// not leave a live cluster degraded.
type undo struct {
	r     *Run
	ctx   context.Context
	names []string
	steps []func(context.Context) error
}

func (r *Run) undoer(ctx context.Context) *undo { return &undo{r: r, ctx: ctx} }

// add registers a restore step under name.
func (u *undo) add(name string, f func(context.Context) error) {
	u.names = append(u.names, name)
	u.steps = append(u.steps, f)
}

// done drops the step named: the phase restored it itself.
func (u *undo) done(name string) {
	for i, n := range u.names {
		if n == name {
			u.names = append(u.names[:i], u.names[i+1:]...)
			u.steps = append(u.steps[:i], u.steps[i+1:]...)
			return
		}
	}
}

// run performs the remaining steps in reverse order.
func (u *undo) run() {
	if len(u.steps) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), 2*time.Minute)
	defer cancel()
	for i, step := range slices.Backward(u.steps) {
		if err := step(ctx); err != nil {
			u.r.event("note", fmt.Sprintf("restore %s failed: %v", u.names[i], err))
			continue
		}
		u.r.event("restore", u.names[i])
	}
	u.steps, u.names = nil, nil
}

// check records a pass/fail with the invariants and lifecycle cells it exercises; failed
// checks may name the file, chunk and node involved.
func (r *Run) check(cites, name string, ok bool, text string) {
	r.emit(Record{Kind: "check", Name: name, Cites: cites, OK: &ok, Text: text})
}

func (r *Run) checkAt(cites, name string, ok bool, text, file, chunk, node string) {
	r.emit(Record{Kind: "check", Name: name, Cites: cites, OK: &ok, Text: text, File: file, Chunk: chunk, Node: node})
}

// repairNow, scrub and gcNow run the maintenance passes and keep the run's totals.
func (r *Run) repairNow(ctx context.Context) (repair.Stats, error) {
	st, err := r.Env.RepairNow(ctx)
	r.addRepair(st)
	return st, err
}

func (r *Run) scrub(ctx context.Context) (repair.Stats, error) {
	st, err := r.Env.Scrub(ctx)
	r.addRepair(st)
	return st, err
}

func (r *Run) addRepair(st repair.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := &r.repair
	t.Scanned += st.Scanned
	t.Repaired += st.Repaired
	t.Partial += st.Partial
	t.Unrepairable += st.Unrepairable
	t.Deferred += st.Deferred
	t.Migrated += st.Migrated
	t.PolicyStale += st.PolicyStale
	t.Scrubbed += st.Scrubbed
	t.ScrubBad += st.ScrubBad
	t.BytesMoved += st.BytesMoved
	t.Errors += st.Errors
}

func (r *Run) gcNow(ctx context.Context) (repair.Result, error) {
	res, err := r.Env.GCNow(ctx)
	r.mu.Lock()
	r.gc.Victims += res.Victims
	r.gc.Deleted += res.Deleted
	r.gc.Refused += res.Refused
	r.mu.Unlock()
	return res, err
}

func (r *Run) skip(reason string) { r.emit(Record{Kind: "phase", Skip: true, Text: reason}) }

// --- measured IO ------------------------------------------------------------------------

type measured struct {
	Volume
	r *Run
}

func (m measured) WriteAt(p string, off int64, data []byte) error {
	t := time.Now()
	err := m.Volume.WriteAt(p, off, data)
	d := time.Since(t)
	m.r.mu.Lock()
	m.r.counters.Writes++
	m.r.counters.BytesWritten += int64(len(data))
	m.r.counters.WriteLatency[bucket(d)]++
	m.r.lats = append(m.r.lats, opLatency{T: t, Phase: m.r.phase, D: d})
	m.r.mu.Unlock()
	return err
}

func (m measured) ReadAll(p string) ([]byte, error) {
	t := time.Now()
	b, err := m.Volume.ReadAll(p)
	d := time.Since(t)
	m.r.mu.Lock()
	m.r.counters.Reads++
	m.r.counters.BytesRead += int64(len(b))
	m.r.counters.ReadLatency[bucket(d)]++
	m.r.lats = append(m.r.lats, opLatency{T: t, Phase: m.r.phase, Read: true, D: d})
	m.r.mu.Unlock()
	return b, err
}

// Hash counts as one read of the file's size.
func (m measured) Hash(p string) ([32]byte, int64, error) {
	t := time.Now()
	sum, n, err := m.Volume.Hash(p)
	d := time.Since(t)
	m.r.mu.Lock()
	m.r.counters.Reads++
	m.r.counters.BytesRead += n
	m.r.counters.ReadLatency[bucket(d)]++
	m.r.lats = append(m.r.lats, opLatency{T: t, Phase: m.r.phase, Read: true, D: d})
	m.r.mu.Unlock()
	return sum, n, err
}

// volume returns the measured gateway of node, or nil when the env does not reach it.
func (r *Run) volume(node NodeRef) Volume {
	v, ok := r.Env.Volume(node)
	if !ok {
		return nil
	}
	return measured{v, r}
}

// gateways lists the nodes whose gateway this driver can use.
func (r *Run) gateways() []NodeRef {
	var out []NodeRef
	for _, n := range r.nodes {
		if _, ok := r.Env.Volume(n); ok {
			out = append(out, n)
		}
	}
	return out
}

// --- sampling -----------------------------------------------------------------------------

func (r *Run) sample(ctx context.Context, round int) {
	s := r.snapshotCounters()
	rep, node, err := r.anyStatus(ctx)
	if err == nil {
		s.Degraded, s.RepairPending, s.Unreadable, s.StalePolicy = rep.Volume.Degraded, rep.Volume.RepairPending, rep.Volume.Unreadable, rep.Volume.StalePolicy
		s.DomainSkew = rep.Volume.DomainSkew
		s.Chunks = rep.Volume.Chunks - rep.Volume.Tombstoned
		if hasDisks(rep) {
			s.Targets = targetsOf(rep)
		}
		s.StoredBytes, s.LogicalBytes, s.DedupRatio = rep.Volume.StoredBytes, rep.Volume.LogicalBytes, rep.Volume.DedupRatio
		s.Health = rep.Health
		s.Slots, s.LiveNodes = map[string]int{}, map[string]bool{}
		for i := range rep.Nodes {
			s.Slots[rep.Nodes[i].Name] = rep.Nodes[i].Slots
			s.LiveNodes[rep.Nodes[i].Name] = rep.Nodes[i].Live
			if rep.Nodes[i].Live {
				s.Live++
			}
		}
		s.RepairBacklog = s.Degraded + s.StalePolicy
		if rep.Gateway != nil {
			s.WALBytes, s.DirtyBytes, s.PlanBytes, s.FlushBytes = rep.Gateway.WALBytes, rep.Gateway.DirtyBytes, rep.Gateway.PlanBytes, rep.Gateway.FlushBytes
		}
		if rep.Process != nil {
			s.HeapInuse, s.Sys, s.Goroutines = rep.Process.HeapInuse, rep.Process.Sys, rep.Process.Goroutines
		}
		if rep.Machine != nil {
			r.vitalsSample(&s, node, *rep.Machine)
		}
	}
	if round%5 == 0 {
		if agree, _, ok := r.fsmAgreement(ctx); ok {
			s.FSMAgree = &agree
		}
	}
	r.emit(Record{Kind: "sample", S: &s, Node: node})
}

// vitalsSample copies a node's machine vitals into the sample and turns its cumulative
// counters into rates against that node's previous sample (docs/12 §8).
func (r *Run) vitalsSample(s *Sample, node string, m admin.MachineReport) {
	now := r.now()
	s.Load1, s.RSS, s.MemAvailable, s.MemTotal, s.MemPressure, s.DiskFree, s.DiskTotal = m.Load1, m.RSS, m.MemAvailable, m.MemTotal, m.MemPressure, m.DiskFree, m.DiskTotal
	if s.RSS == 0 {
		s.RSS = m.RSSPeak // the peak is the best a macOS node offers
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vitals == nil {
		r.vitals = map[string]vitalsPrev{}
	}
	prev, ok := r.vitals[node]
	if !ok {
		r.vitals[node] = vitalsPrev{t: now, m: m}
		return
	}
	if dt := now.Sub(prev.t); dt < minRateInterval {
		// too close to divide by: carry the last rates, keep the earlier report as the base
		s.CPUPct, s.MachinePct, s.DiskWriteBps, s.DiskReadBps = prev.cpu, prev.mcpu, prev.wr, prev.rd
		return
	}
	dt := now.Sub(prev.t).Seconds()
	cur := vitalsPrev{t: now, m: m, hasRates: true}
	if m.CPUSeconds >= prev.m.CPUSeconds {
		cur.cpu = (m.CPUSeconds - prev.m.CPUSeconds) / dt * 100
	}
	if m.MachineTotal > prev.m.MachineTotal && m.MachineBusy >= prev.m.MachineBusy {
		cur.mcpu = float64(m.MachineBusy-prev.m.MachineBusy) / float64(m.MachineTotal-prev.m.MachineTotal) * 100
	}
	if m.DiskWritten >= prev.m.DiskWritten {
		cur.wr = float64(m.DiskWritten-prev.m.DiskWritten) / dt
	}
	if m.DiskRead >= prev.m.DiskRead {
		cur.rd = float64(m.DiskRead-prev.m.DiskRead) / dt
	}
	r.vitals[node] = cur
	s.CPUPct, s.MachinePct, s.DiskWriteBps, s.DiskReadBps = cur.cpu, cur.mcpu, cur.wr, cur.rd
}

func (r *Run) snapshotCounters() Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters
}

// setPhase names the phase every record emitted from now on belongs to (the sampler
// goroutine reads it).
func (r *Run) setPhase(name string) {
	r.mu.Lock()
	r.phase = name
	r.mu.Unlock()
}

// recordsSince copies the records appended at or after index from (the sampler appends
// concurrently).
func (r *Run) recordsSince(from int) []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.records[from:]...)
}

func (r *Run) recordCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

// anyStatus returns the leader's status report when the leader answers (it carries the
// repair view and a leader is healthy by definition), else the first node's that does.
func (r *Run) anyStatus(ctx context.Context) (admin.Report, string, error) {
	var last error
	var first *admin.Report
	var from string
	for _, n := range r.nodes {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		rep, err := r.Env.Status(cctx, n)
		cancel()
		if err != nil {
			last = err
			continue
		}
		r.learnHosts(rep)
		if rep.Leader != "" && rep.Node == rep.Leader {
			return rep, n.Name, nil
		}
		if first == nil {
			first, from = &rep, n.Name
		}
	}
	if first != nil {
		return *first, from, nil
	}
	return admin.Report{}, "", last
}

// fsmAgreement compares the FSM hashes of every answering node at one applied index. It
// returns ok=false when fewer than two nodes answer or they are at different indexes.
func (r *Run) fsmAgreement(ctx context.Context) (agree bool, detail string, ok bool) {
	type h struct {
		node    string
		applied uint64
		hash    string
	}
	var hs []h
	for _, n := range r.nodes {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		applied, hash, err := r.Env.FSMHash(cctx, n)
		cancel()
		if err == nil {
			hs = append(hs, h{n.Name, applied, hash})
		}
	}
	if len(hs) < 2 {
		return false, "fewer than two nodes answered", false
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].node < hs[j].node })
	for _, x := range hs[1:] {
		if x.applied != hs[0].applied {
			return false, fmt.Sprintf("indexes differ (%s@%d vs %s@%d)", hs[0].node, hs[0].applied, x.node, x.applied), false
		}
	}
	for _, x := range hs[1:] {
		if x.hash != hs[0].hash {
			return false, fmt.Sprintf("%s and %s differ at applied %d", hs[0].node, x.node, x.applied), true
		}
	}
	return true, fmt.Sprintf("%d nodes agree at applied %d", len(hs), hs[0].applied), true
}

// fsmAgreementSettled retries until every node answers at one index (quiescent cluster).
// available is false when fewer than two nodes serve /dev/fsm-hash at all.
func (r *Run) fsmAgreementSettled(ctx context.Context) (agree bool, detail string, available bool) {
	for range 20 {
		var ok bool
		agree, detail, ok = r.fsmAgreement(ctx)
		if ok {
			return agree, detail, true
		}
		if strings.HasPrefix(detail, "fewer than two") {
			return false, detail, false
		}
		if err := r.Env.Wait(ctx, 500*time.Millisecond); err != nil {
			break
		}
	}
	return agree, detail, true
}

func (r *Run) startSampler(ctx context.Context) {
	if r.Profile.SampleEvery <= 0 {
		return
	}
	r.stopSamp, r.sampDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(r.sampDone)
		tick := time.NewTicker(r.Profile.SampleEvery)
		defer tick.Stop()
		for round := 0; ; round++ {
			r.sample(ctx, round)
			select {
			case <-r.stopSamp:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

func (r *Run) stopSampler() {
	if r.stopSamp != nil {
		close(r.stopSamp)
		<-r.sampDone
	}
}

// --- execution ---------------------------------------------------------------------------

// Execute runs every phase in order and returns the summary; the error is only for a run
// that could not start (no nodes, no gateway).
func (r *Run) Execute(ctx context.Context) (Summary, error) {
	if r.Rng == nil {
		r.Rng = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // test workload
	}
	if r.Root == "" {
		r.Root = "/_soak/" + time.Now().UTC().Format("20060102T150405")
	}
	nodes, err := r.Env.Nodes(ctx)
	if err != nil {
		return r.abort(err)
	}
	r.nodes = nodes
	r.started = r.now()
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return r.abort(fmt.Errorf("soak: status: %w", err))
	}
	if r.Profile.ChunkSize == 0 { // the corpus straddles the cluster's real chunk size
		if rep.Volume.ChunkSize == 0 {
			return r.abort(errors.New("soak: status reports no chunk size"))
		}
		r.Profile.ChunkSize = int(rep.Volume.ChunkSize)
	}
	sum := Summary{
		Env: r.Env.Name(), Profile: r.Profile.Name, Seed: r.Seed, Flags: r.Flags, Started: r.started, Nodes: nodes,
		K: rep.Volume.K, M: rep.Volume.M, ChunkSize: r.Profile.ChunkSize, NodeCount: len(nodes), Versions: map[string]string{},
	}
	for i := range rep.Nodes {
		sum.Versions[rep.Nodes[i].Name] = rep.Nodes[i].Version
	}
	if len(r.gateways()) == 0 {
		return r.abort(fmt.Errorf("soak: no gateway reachable in %s", r.Env.Name()))
	}
	gw := r.gateways()[0]
	v := r.volume(gw)
	for range 30 { // a fresh cluster may still be electing (EROFS)
		if err = v.Mkdir(r.Root); err == nil {
			break
		}
		if werr := r.Env.Wait(ctx, time.Second); werr != nil {
			break
		}
	}
	if err != nil {
		return r.abort(fmt.Errorf("soak: mkdir %s: %w", r.Root, err))
	}
	r.setShared("corpus", model{}) // phase 1 fills it; a phase that runs first or after a failed phase 1 finds a map, not nil
	r.startSampler(ctx)
	var results []PhaseResult
	table := r.phaseList()
	for i, ph := range table {
		if ctx.Err() != nil { // cancelled: the rest is NOT RUN, and the report says so
			for _, rest := range table[i:] {
				results = append(results, PhaseResult{Name: rest.name, NotRun: true})
			}
			after := "the start"
			if i > 0 {
				after = table[i-1].name
			}
			sum.Aborted = "cancelled after " + after + ": " + ctx.Err().Error()
			break
		}
		results = append(results, r.runPhase(ctx, ph))
	}
	if ctx.Err() != nil && sum.Aborted == "" {
		sum.Aborted = "cancelled: " + ctx.Err().Error()
	}
	r.setPhase("cleanup")
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute) // a cancelled run still tidies the volume
	r.cleanup(cctx)
	ccancel()
	r.stopSampler()
	r.emit(Record{Kind: "summary", Text: fmt.Sprintf("%d failed checks", r.failed)})
	r.mu.Lock()
	defer r.mu.Unlock()
	sum.Duration, sum.Phases, sum.Failed = r.now().Sub(r.started), results, r.failed
	sum.Records = append([]Record(nil), r.records...)
	for _, pr := range results {
		sum.Partial = sum.Partial || pr.Skipped
	}
	sum.Latency = latencyRows(r.lats, results, faultWindows(sum.Records))
	sum.Coverage = coverage(results)
	sum.Totals = Totals{BytesWritten: r.counters.BytesWritten, BytesRead: r.counters.BytesRead, Writes: r.counters.Writes, Reads: r.counters.Reads, Repair: r.repair, GC: r.gc}
	for i := range sum.Records {
		s := sum.Records[i].S
		if s == nil {
			continue
		}
		sum.Totals.MaxWALBytes = max(sum.Totals.MaxWALBytes, s.WALBytes)
		sum.Totals.MaxFlush = max(sum.Totals.MaxFlush, s.FlushBytes)
		sum.Totals.MaxHeap = max(sum.Totals.MaxHeap, s.HeapInuse)
		sum.Totals.MaxRSS = max(sum.Totals.MaxRSS, s.RSS)
		sum.Totals.MaxCPUPct = max(sum.Totals.MaxCPUPct, s.CPUPct)
		if s.DiskFree > 0 && (sum.Totals.MinDiskFree == 0 || s.DiskFree < sum.Totals.MinDiskFree) {
			sum.Totals.MinDiskFree = s.DiskFree
		}
		if s.DedupRatio > 0 {
			sum.Totals.DedupRatio = s.DedupRatio
		}
	}
	if sum.Aborted != "" { // the summary is complete as far as it goes; the caller still exits non-zero
		return sum, errors.New("soak: " + sum.Aborted)
	}
	return sum, nil
}

// StandardPhases is the phase table of docs/09 §6 whose numbers are listed ("15",
// "1,13"); every phase when none is. A driver that runs a subset passes it as Run.Phases.
func StandardPhases(numbers ...string) []Phase {
	var out []Phase
	for _, ph := range phases {
		num, _, _ := strings.Cut(ph.name, " ")
		if len(numbers) == 0 || slices.Contains(numbers, num) {
			out = append(out, Phase{Name: ph.name, Run: ph.run})
		}
	}
	return out
}

// phaseList is the phases this run executes: the standard table, or Run.Phases.
func (r *Run) phaseList() []phase {
	if r.Phases == nil {
		return phases
	}
	out := make([]phase, len(r.Phases))
	for i, ph := range r.Phases {
		out[i] = phase{name: ph.Name, run: ph.Run}
	}
	return out
}

// runPhase executes one phase and records it. A panic in the phase is a failed check with
// the stack as a dump, not a dead driver; a phase longer than PhaseTimeout is failed, its
// context cancelled and the run moves on, with every node's goroutines dumped first. A
// phase that recorded no check and did not skip fails: silence is not a pass.
func (r *Run) runPhase(ctx context.Context, ph phase) PhaseResult {
	r.setPhase(ph.name)
	before := r.recordCount()
	t := r.now()
	r.emit(Record{Kind: "phase", Name: ph.name, Text: "start"})
	r.sample(ctx, 0)
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				r.check("", "phase completed", false, fmt.Sprintf("panic: %v", p))
				r.emit(Record{Kind: "dump", Node: "driver", Text: string(debug.Stack())})
			}
		}()
		if err := ph.run(pctx, r); err != nil && pctx.Err() == nil { // after a timeout the failure is already recorded
			r.check("", "phase completed", false, err.Error())
		}
	}()
	var expired <-chan time.Time
	if r.Profile.PhaseTimeout > 0 {
		tm := time.NewTimer(r.Profile.PhaseTimeout)
		defer tm.Stop()
		expired = tm.C
	}
	select {
	case <-done:
	case <-expired:
		r.check("", "phase completed", false, fmt.Sprintf("timed out after %s", r.Profile.PhaseTimeout))
		r.captureDumps(ctx) // what every node was doing while the phase hung
		cancel()
		select { // a cooperative phase returns at once; a moment's grace keeps its last records in its own phase
		case <-done:
		case <-time.After(time.Second):
		}
	}
	r.sample(ctx, 0)
	pr := PhaseResult{Name: ph.name, Took: r.now().Sub(t)}
	collect := func() {
		pr.Checks, pr.Failed, pr.Skipped, pr.Reason = nil, 0, false, ""
		recs := r.recordsSince(before)
		for i := range recs {
			rec := &recs[i]
			switch {
			case rec.Kind == "check":
				pr.Checks = append(pr.Checks, *rec)
				if rec.OK != nil && !*rec.OK {
					pr.Failed++
				}
			case rec.Kind == "phase" && rec.Skip:
				pr.Skipped, pr.Reason = true, rec.Text
			}
		}
	}
	collect()
	if !pr.Skipped && len(pr.Checks) == 0 {
		r.check("", "phase recorded checks", false, "no checks: the phase asserted nothing")
		collect()
	}
	if pr.Failed > 0 {
		r.captureDumps(ctx)
	}
	return pr
}

// abort is the summary of a run that stopped before its first phase: the records so far
// and the reason, so the report still renders (verdict ABORTED).
func (r *Run) abort(err error) (Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Summary{
		Env: r.Env.Name(), Profile: r.Profile.Name, Seed: r.Seed, Flags: r.Flags, Started: r.started,
		Duration: r.now().Sub(r.started), Aborted: err.Error(), Records: append([]Record(nil), r.records...), Versions: map[string]string{},
	}, err
}

// dumpMax bounds one node's goroutine dump in the results stream; dumpTotalMax bounds a
// run's dumps altogether (N nodes × failed phases × dumpMax would not).
const (
	dumpMax      = 512 << 10
	dumpTotalMax = 4 << 20
)

// captureDumps records every node's goroutine dump after a failed phase (docs/09 §6); a
// node without dev.enabled, or one that does not answer, leaves a note instead.
func (r *Run) captureDumps(ctx context.Context) {
	for _, n := range r.nodes {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second) // after a timeout the phase context is gone; the dump is the point
		dump, err := r.Env.Goroutines(cctx, n)
		cancel()
		if err != nil {
			r.event("note", fmt.Sprintf("no goroutine dump from %s: %v", n.Name, err))
			continue
		}
		if len(dump) > dumpMax {
			dump = dump[:dumpMax] + "\n... (truncated)"
		}
		r.mu.Lock()
		over := r.dumpBytes+len(dump) > dumpTotalMax
		if !over {
			r.dumpBytes += len(dump)
		}
		r.mu.Unlock()
		if over {
			r.event("note", fmt.Sprintf("goroutine dump from %s omitted: the run's dumps already hold %s", n.Name, humanBytes(float64(r.dumpBytes))))
			continue
		}
		r.emit(Record{Kind: "dump", Node: n.Name, Text: dump})
	}
}

// cleanup removes the run directory so a live volume is left as found.
func (r *Run) cleanup(ctx context.Context) {
	_ = ctx
	gws := r.gateways()
	if len(gws) == 0 {
		return
	}
	v := r.volume(gws[0])
	removeTree(v, r.Root)
}

// removeTree deletes everything under p (best effort, used for cleanup).
func removeTree(v Volume, p string) {
	if ents, err := v.List(p); err == nil {
		for _, e := range ents {
			removeTree(v, join(p, e))
		}
	}
	_ = v.Remove(p)
}
