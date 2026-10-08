package soak

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
)

// Profile sizes a run.
type Profile struct {
	Name          string
	BigFile       int           // bytes of the largest corpus file
	SmallFiles    int           // count of tiny files
	ChurnOps      int           // random-access operations per gateway
	LeaderKills   int           // consensus phase rounds
	FlushIdle     time.Duration // how long to wait for the idle flush after writes
	FillAfter     time.Duration // the targets' repair.fill_after (parity phase waits this)
	Grace         time.Duration // the targets' repair.grace (GC phase waits this)
	SampleEvery   time.Duration // metric sampling period (0 = phase boundaries only)
	ChunkSize     int           // the targets' chunk size (corpus sizes straddle it)
	RestartSettle time.Duration // how long nodes take to come back after a restart
}

// Quick is the compose profile (~10 min).
func Quick() Profile {
	return Profile{Name: "quick", BigFile: 3_000_000, SmallFiles: 150, ChurnOps: 300, LeaderKills: 3, ChunkSize: 256 << 10,
		FlushIdle: 4 * time.Second, FillAfter: 20 * time.Second, Grace: 20 * time.Second, SampleEvery: time.Second, RestartSettle: 15 * time.Second}
}

// Record is one line of results.jsonl.
type Record struct {
	T     time.Time         `json:"t"`
	Kind  string            `json:"kind"` // sample | event | check | phase | summary
	Phase string            `json:"phase,omitempty"`
	Name  string            `json:"name,omitempty"`
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
	BytesWritten  int64          `json:"bytes_written"`
	BytesRead     int64          `json:"bytes_read"`
	Writes        int64          `json:"writes"`
	Reads         int64          `json:"reads"`
	WriteLatency  [8]int64       `json:"write_latency"` // histogram buckets, see latencyBuckets
	ReadLatency   [8]int64       `json:"read_latency"`
	Degraded      int            `json:"degraded"`
	Unreadable    int            `json:"unreadable"`
	StalePolicy   int            `json:"stale_policy"`
	StoredBytes   uint64         `json:"stored_bytes"`
	LogicalBytes  uint64         `json:"logical_bytes"`
	Slots         map[string]int `json:"slots"` // per node name
	Live          int            `json:"live"`
	FSMAgree      *bool          `json:"fsm_agree,omitempty"` // nil when not sampled this round
	Health        string         `json:"health"`
	RepairBacklog int            `json:"repair_backlog"` // degraded + stale policy
}

// latencyBuckets are the upper bounds of the latency histogram (ms), last is +Inf.
var latencyBuckets = [8]float64{1, 5, 20, 50, 100, 500, 2000, 0}

func bucket(d time.Duration) int {
	ms := float64(d) / float64(time.Millisecond)
	for i := 0; i < len(latencyBuckets)-1; i++ {
		if ms <= latencyBuckets[i] {
			return i
		}
	}
	return len(latencyBuckets) - 1
}

// Run drives one profile against one Env and collects results.
type Run struct {
	Env     Env
	Profile Profile
	Log     *slog.Logger
	Out     io.Writer // results.jsonl (may be nil)
	Rng     *rand.Rand
	Root    string // volume directory for this run, e.g. /_soak/20261008T1200

	mu       sync.Mutex
	records  []Record
	counters Sample
	phase    string
	nodes    []NodeRef
	failed   int
	started  time.Time
	stopSamp chan struct{}
	sampDone chan struct{}
	shared   map[string]any // cross-phase state (the corpus model)
}

// Summary is the outcome of a run.
type Summary struct {
	Env      string
	Profile  string
	Started  time.Time
	Duration time.Duration
	Phases   []PhaseResult
	Failed   int
	Records  []Record
	Nodes    []NodeRef
}

// PhaseResult aggregates the checks of one phase.
type PhaseResult struct {
	Name    string
	Checks  []Record
	Skipped bool
	Reason  string
	Failed  int
	Took    time.Duration
}

func (r *Run) now() time.Time { return time.Now() }

func (r *Run) emit(rec Record) {
	rec.T = r.now()
	r.mu.Lock()
	if rec.Phase == "" {
		rec.Phase = r.phase
	}
	r.records = append(r.records, rec)
	if rec.Kind == "check" && rec.OK != nil && !*rec.OK {
		r.failed++
	}
	if r.Out != nil {
		b, _ := json.Marshal(rec)
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

// check records a pass/fail; failed checks may name the file, chunk and node involved.
func (r *Run) check(name string, ok bool, text string) bool {
	r.emit(Record{Kind: "check", Name: name, OK: &ok, Text: text})
	return ok
}

func (r *Run) checkAt(name string, ok bool, text, file, chunk, node string) bool {
	r.emit(Record{Kind: "check", Name: name, OK: &ok, Text: text, File: file, Chunk: chunk, Node: node})
	return ok
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
	m.r.mu.Lock()
	m.r.counters.Writes++
	m.r.counters.BytesWritten += int64(len(data))
	m.r.counters.WriteLatency[bucket(time.Since(t))]++
	m.r.mu.Unlock()
	return err
}

func (m measured) ReadAll(p string) ([]byte, error) {
	t := time.Now()
	b, err := m.Volume.ReadAll(p)
	m.r.mu.Lock()
	m.r.counters.Reads++
	m.r.counters.BytesRead += int64(len(b))
	m.r.counters.ReadLatency[bucket(time.Since(t))]++
	m.r.mu.Unlock()
	return b, err
}

// volume returns the measured gateway of node, if the env reaches it.
func (r *Run) volume(node NodeRef) (Volume, bool) {
	v, ok := r.Env.Volume(node)
	if !ok {
		return nil, false
	}
	return measured{v, r}, true
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
		s.Degraded, s.Unreadable, s.StalePolicy = rep.Volume.Degraded, rep.Volume.Unreadable, rep.Volume.StalePolicy
		s.StoredBytes, s.LogicalBytes = rep.Volume.StoredBytes, rep.Volume.LogicalBytes
		s.Health = rep.Health
		s.Slots = map[string]int{}
		for i := range rep.Nodes {
			s.Slots[rep.Nodes[i].Name] = rep.Nodes[i].Slots
			if rep.Nodes[i].Live {
				s.Live++
			}
		}
		s.RepairBacklog = s.Degraded + s.StalePolicy
	}
	if round%5 == 0 {
		if agree, _, ok := r.fsmAgreement(ctx); ok {
			s.FSMAgree = &agree
		}
	}
	r.emit(Record{Kind: "sample", S: &s, Node: node})
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
	for attempt := 0; attempt < 20; attempt++ {
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
		return Summary{}, err
	}
	r.nodes = nodes
	r.started = r.now()
	if r.Profile.ChunkSize == 0 { // the corpus straddles the cluster's real chunk size
		rep, _, err := r.anyStatus(ctx)
		if err != nil || rep.Volume.ChunkSize == 0 {
			return Summary{}, fmt.Errorf("soak: chunk size from status (%d): %w", rep.Volume.ChunkSize, err)
		}
		r.Profile.ChunkSize = int(rep.Volume.ChunkSize)
	}
	if len(r.gateways()) == 0 {
		return Summary{}, fmt.Errorf("soak: no gateway reachable in %s", r.Env.Name())
	}
	gw := r.gateways()[0]
	v, _ := r.volume(gw)
	for attempt := 0; attempt < 30; attempt++ { // a fresh cluster may still be electing (EROFS)
		if err = v.Mkdir(r.Root); err == nil {
			break
		}
		if werr := r.Env.Wait(ctx, time.Second); werr != nil {
			break
		}
	}
	if err != nil {
		return Summary{}, fmt.Errorf("soak: mkdir %s: %w", r.Root, err)
	}
	r.startSampler(ctx)
	var results []PhaseResult
	for _, ph := range phases {
		r.setPhase(ph.name)
		before := r.recordCount()
		t := r.now()
		r.emit(Record{Kind: "phase", Name: ph.name, Text: "start"})
		r.sample(ctx, 0)
		if err := ph.run(ctx, r); err != nil {
			r.check("phase completed", false, err.Error())
		}
		r.sample(ctx, 0)
		pr := PhaseResult{Name: ph.name, Took: r.now().Sub(t)}
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
		results = append(results, pr)
		if ctx.Err() != nil {
			break
		}
	}
	r.setPhase("cleanup")
	r.cleanup(ctx)
	r.stopSampler()
	r.emit(Record{Kind: "summary", Text: fmt.Sprintf("%d failed checks", r.failed)})
	r.mu.Lock()
	sum := Summary{Env: r.Env.Name(), Profile: r.Profile.Name, Started: r.started, Duration: r.now().Sub(r.started), Phases: results, Failed: r.failed, Records: append([]Record(nil), r.records...), Nodes: r.nodes}
	r.mu.Unlock()
	return sum, nil
}

// cleanup removes the run directory so a live volume is left as found.
func (r *Run) cleanup(ctx context.Context) {
	_ = ctx
	gws := r.gateways()
	if len(gws) == 0 {
		return
	}
	v, _ := r.volume(gws[0])
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
