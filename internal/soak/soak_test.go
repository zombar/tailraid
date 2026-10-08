package soak_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/zombar/tailraid/internal/testseed"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/dev"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/testharness"
)

// harnessEnv runs the soak phases against the in-process cluster: billy volumes, the fake
// clock behind Wait, and the admin surface called directly.
type harnessEnv struct {
	t           *testing.T
	c           *testharness.Cluster
	partitioned bool // Settle would wait for cut-off nodes to catch up, which they cannot

	// a restart rebuilds a node's overlay and FSM handles while the sampler goroutine
	// reads them: faults take the write side, the sampler's reads the read side
	mu sync.RWMutex
}

func (e *harnessEnv) Name() string { return "in-process harness" }

func (e *harnessEnv) Nodes(context.Context) ([]soak.NodeRef, error) {
	out := make([]soak.NodeRef, 0, len(e.c.Nodes))
	for _, nd := range e.c.Nodes {
		out = append(out, soak.NodeRef{Name: fmt.Sprintf("n%d", nd.I), Idx: nd.Idx(), RPC: nd.RPCAddr()})
	}
	return out, nil
}

func (e *harnessEnv) node(n soak.NodeRef) *testharness.ClusterNode { return e.c.Nodes[int(n.Idx)-1] }

func (e *harnessEnv) Volume(n soak.NodeRef) (soak.Volume, bool) {
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return nil, false
	}
	return soak.BillyVolume{FS: nd.FS}, true
}

func (e *harnessEnv) Caps() soak.Caps {
	return soak.Caps{Kill: true, Partition: true, Corrupt: true, MultiGateway: true}
}

func (e *harnessEnv) quorum() bool {
	alive := 0
	for i := range e.c.Nodes {
		if e.c.Alive(i) {
			alive++
		}
	}
	return alive > len(e.c.Nodes)/2
}

// Wait advances the fake clock, flushes every live gateway and settles the cluster
// (not under a partition: the cut-off side cannot catch up until the heal).
func (e *harnessEnv) Wait(ctx context.Context, d time.Duration) error {
	e.c.Clk.Advance(d)
	e.c.Probe()
	if !e.quorum() {
		return nil
	}
	for i := range e.c.Nodes {
		if e.c.Alive(i) {
			_ = e.c.Nodes[i].Flush.FlushAll(ctx)
		}
	}
	if !e.partitioned {
		e.c.Settle()
	}
	return nil
}

func (e *harnessEnv) Status(_ context.Context, n soak.NodeRef) (admin.Report, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return admin.Report{}, errors.New("node is down")
	}
	rep := admin.Build(nd.Node, nd.Prober, e.c.CID, e.c.Clk, nil)
	rep.Gateway = admin.GatewayFromDirty(nd.Ov.Dirty())
	return rep, nil
}

func (e *harnessEnv) repairer(scrub bool) *repair.Repairer {
	cfg := repair.DefaultConfig()
	cfg.RateBytes = 0
	cfg.ScrubShare = 0
	if scrub {
		cfg.ScrubShare = 1
	}
	l := e.c.Leader()
	r := repair.New(l.Node.FSM, l.Node, l.Prober, l.Client, l.Reader, e.c.Clk, cfg, nil)
	r.Events = l.Events
	return r
}

func (e *harnessEnv) RepairNow(ctx context.Context) (repair.Stats, error) {
	st, err := e.repairer(false).Scan(ctx)
	e.c.Settle()
	return st, err
}

func (e *harnessEnv) Scrub(ctx context.Context) (repair.Stats, error) {
	st, err := e.repairer(true).Scan(ctx)
	e.c.Settle()
	return st, err
}

func (e *harnessEnv) GCNow(ctx context.Context) (repair.Result, error) {
	l := e.c.Leader()
	return repair.NewGC(l.Node.FSM, l.Node, l.Client, l.Prober, e.c.Clk, 10*time.Minute, nil).RunOnce(ctx)
}

func (e *harnessEnv) SetPolicy(ctx context.Context, k, m uint8) error {
	l := e.c.Leader()
	res, _, err := l.Node.Propose(ctx, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: k, M: m, Now: e.c.Clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	e.c.Settle()
	return err
}

func (e *harnessEnv) SetState(ctx context.Context, n soak.NodeRef, st meta.NodeState) error {
	err := e.c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: n.Idx, State: st})
	e.c.Settle()
	return err
}

func (e *harnessEnv) Fsck(_ context.Context, n soak.NodeRef) (bool, string, error) {
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return false, "", errors.New("node is down")
	}
	snap, err := nd.Node.FSM.Snapshot()
	if err != nil {
		return false, "", err
	}
	v := meta.CheckInvariants(snap.(interface{ State() *meta.State }).State())
	return len(v) == 0, strings.Join(v, "; "), nil
}

func (e *harnessEnv) Chunks(ctx context.Context, n soak.NodeRef, path string) ([]admin.ChunkReport, error) {
	nd := e.node(n)
	return admin.DescribeChunks(ctx, nd.Node.FSM, nd.Prober, nd.Client, nd.Events, path)
}

func (e *harnessEnv) FSMHash(_ context.Context, n soak.NodeRef) (uint64, string, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return 0, "", errors.New("node is down")
	}
	h, err := dev.Hash(nd.Node.FSM)
	return h.Applied, h.Hash, err
}

// Goroutines dumps the test process (every harness node lives in it).
func (e *harnessEnv) Goroutines(context.Context, soak.NodeRef) (string, error) {
	var b bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&b, 2); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (e *harnessEnv) Kill(_ context.Context, n soak.NodeRef) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.c.Kill(e.node(n).I)
	e.c.Clk.Advance(cluster.LiveThreshold + time.Second)
	e.c.Probe()
	return nil
}

func (e *harnessEnv) Start(_ context.Context, n soak.NodeRef) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.c.Restart(e.node(n).I)
	if e.quorum() {
		e.c.Settle()
	}
	return nil
}

func (e *harnessEnv) Restart(ctx context.Context, nodes []soak.NodeRef) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range nodes {
		e.c.Kill(e.node(n).I)
	}
	for _, n := range nodes {
		e.c.Restart(e.node(n).I)
	}
	e.c.Settle()
	return nil
}

func (e *harnessEnv) Partition(_ context.Context, a, b []soak.NodeRef) error {
	ia, ib := make([]int, 0, len(a)), make([]int, 0, len(b))
	for _, n := range a {
		ia = append(ia, e.node(n).I)
	}
	for _, n := range b {
		ib = append(ib, e.node(n).I)
	}
	e.c.Partition(ia, ib)
	e.partitioned = true
	return nil
}

func (e *harnessEnv) Heal(context.Context) error {
	e.c.Heal()
	e.partitioned = false
	e.c.Settle()
	return nil
}

func (e *harnessEnv) Corrupt(_ context.Context, n soak.NodeRef, key string) error {
	nd := e.node(n)
	if !nd.Store.Has(chunk.ShardKey(key)) {
		return errors.New("no such shard")
	}
	return dev.Corrupt(nd.Store.Path(chunk.ShardKey(key)))
}

func harnessProfile() soak.Profile {
	cs := int(testharness.ChunkSize)
	return soak.Profile{Name: "harness", BigFile: 3*cs + 1000, SmallFiles: 8, ChurnOps: 40, LeaderKills: 1, ChunkSize: cs,
		FlushIdle: time.Second, FillAfter: cluster.FillAfter + time.Second, Grace: 10*time.Minute + time.Second, RestartSettle: time.Second,
		SampleEvery: 50 * time.Millisecond} // the sampler runs concurrently with the phases, as it does in production
}

// The whole scenario sequence runs green against the in-process cluster, every phase
// reports checks (none skipped), the results stream has samples and events, and the
// report renders with the phase matrix and charts. The partition phase is the only
// end-to-end exercise of a cut-off gateway: metadata refused, WRITEs buffered and
// committed after the heal, the majority committing throughout.
// Invariant: I-29
// Lifecycle: L-59, L-148
func TestSoak_AllPhasesAgainstHarness(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	env := &harnessEnv{t: t, c: c}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ph := range sum.Phases {
		if ph.Skipped {
			t.Errorf("%s skipped: %s", ph.Name, ph.Reason)
		}
		for _, chk := range ph.Checks {
			if chk.OK != nil && !*chk.OK {
				t.Errorf("%s: %s: %s", ph.Name, chk.Name, chk.Text)
			}
		}
		if len(ph.Checks) == 0 {
			t.Errorf("%s has no checks", ph.Name)
		}
	}
	if sum.Failed != 0 || len(sum.Phases) != 10 {
		t.Fatalf("summary: %d failed, %d phases", sum.Failed, len(sum.Phases))
	}
	samples, events := 0, 0
	for _, rec := range sum.Records {
		switch rec.Kind {
		case "sample":
			samples++
		case "event":
			events++
		}
	}
	if samples < 9 || events < 5 || !strings.Contains(jsonl.String(), `"kind":"summary"`) {
		t.Fatalf("results: %d samples, %d events", samples, events)
	}
	var html bytes.Buffer
	if err := soak.Render(&html, sum); err != nil {
		t.Fatal(err)
	}
	out := html.String()
	if path := os.Getenv("SOAK_REPORT_OUT"); path != "" { // for eyeballing the report
		_ = os.WriteFile(path, html.Bytes(), 0o600)
	}
	for _, want := range []string{"<svg", "9 restart storm", "10 partition", ">PASS —", "FSM-hash agreement", "Throughput", fmt.Sprintf("seed <code>%d</code>", seed),
		"policy 2&#43;1, chunk", "4 nodes", "Node liveness", "under faults", "I-12", "L-59", "Overlay WAL depth", "refused metadata operation"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(out, "peak write 0.0 MiB/s") {
		t.Error("throughput chart has no rate: the boundary-sample filter dropped every interval")
	}
	for _, want := range []string{} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if sum.Partial || sum.K != 2 || sum.M != 1 || sum.NodeCount != 4 || sum.Totals.BytesWritten == 0 || sum.Totals.Repair.Scanned == 0 || sum.Totals.GC.Victims == 0 || len(sum.Coverage) < 30 {
		t.Errorf("summary metadata: partial=%v %d+%d nodes=%d totals=%+v coverage=%d", sum.Partial, sum.K, sum.M, sum.NodeCount, sum.Totals, len(sum.Coverage))
	}
	for i, rec := range sum.Records {
		if rec.Line != i+1 {
			t.Fatalf("record %d has line %d", i, rec.Line)
		}
	}
	// the cleanup left no run directory behind
	if _, err := c.Nodes[0].FS.Stat(run.Root); err == nil {
		t.Fatalf("run directory %s still exists", run.Root)
	}
}
