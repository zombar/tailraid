package soak_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testseed"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/dev"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
	"github.com/zombar/tailraid/internal/s3/s3test"
	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/stats"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
	"github.com/zombar/tailraid/internal/webui"
)

// harnessEnv runs the soak phases against the in-process cluster: billy volumes, the fake
// clock behind Wait, and the admin surface called directly.
type harnessEnv struct {
	t           *testing.T
	c           *testharness.Cluster
	partitioned bool // Settle would wait for cut-off nodes to catch up, which they cannot

	// a restart rebuilds a node's overlay and FSM handles while the sampler goroutine
	// reads them: faults take the write side, the sampler's reads the read side
	mu   sync.RWMutex
	fill map[int]uint64 // scratch bytes "on" each node's fake disk (Fill)

	bucket *s3test.Server // the bucket phase 15 registers; nil: Caps.S3 is false

	// the leader's repairer, kept while that node leads so the home gate carries over
	rep     *repair.Repairer
	repNode *cluster.Node

	// one browser UI per node, built on WebURL over the node's current handles (a restart
	// rebuilds them), with that node's pins and their worker (docs/13, phase 16)
	web  map[int]*httptest.Server
	pins map[int]*pins.Pins
	stop map[int]context.CancelFunc
	once sync.Once
	// the stats sampler of docs/17 on every node, as serve runs it, on the fake clock
	stats map[int]*harnessStats
	// the index of docs/18 on every node, rebuilt when a restart replaced its handles
	search map[int]*harnessSearch
	// phase 19's mirror once AddMirror registered it (mirror_harness_test.go)
	mirror *harnessMirror
	// one SMB gateway per node, shared by every copy job and handle race (phases 22, 23),
	// rebuilt when a restart replaced the node's filesystem
	smb map[int]*harnessSMB
}

type harnessSearch struct {
	fs  *vfs.FS
	svc *search.Service
}

// searchOf is node i's index service over its current handles, as serve builds it.
func (e *harnessEnv) searchOf(nd *testharness.ClusterNode) *search.Service {
	if e.search == nil {
		e.search = map[int]*harnessSearch{}
	}
	if h := e.search[nd.I]; h != nil && h.fs == nd.FS {
		return h.svc
	}
	svc := &search.Service{
		Src: nd.Node.FSM, Clk: e.c.Clk, Cfg: search.DefaultConfig(), Log: slog.New(slog.DiscardHandler),
		Vol: search.VFS{FS: nd.FS, St: nd.Node.FSM, Rd: vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil)},
	}
	e.search[nd.I] = &harnessSearch{fs: nd.FS, svc: svc}
	return svc
}

// IndexNow runs the leader's pass (docs/18 §5).
func (e *harnessEnv) IndexNow(ctx context.Context) (search.PassResult, error) {
	l := e.c.Leader()
	e.mu.Lock()
	svc := e.searchOf(l)
	e.mu.Unlock()
	return svc.Pass(ctx)
}

type harnessStats struct {
	svc    *stats.Service
	cancel context.CancelFunc
	done   chan struct{}
}

// startStats runs node i's sampler: one row a second on the fake clock, sealed into the
// volume through the node's own vfs, the cluster rows while it leads.
func (e *harnessEnv) startStats(i int) {
	if e.stats == nil {
		e.stats = map[int]*harnessStats{}
	}
	nd := e.c.Nodes[i]
	vol := stats.VFS{FS: nd.FS, St: nd.Node.FSM, Rd: vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil)}
	name := fmt.Sprintf("n%d", nd.I)
	svc := &stats.Service{
		Name: name, Every: time.Second, Keep: stats.DefaultKeep, Clk: e.c.Clk,
		Writer: &stats.Writer{Vol: vol, Name: name, Log: slog.New(slog.DiscardHandler)}, Log: slog.New(slog.DiscardHandler),
		Src: stats.Sources{
			Machine: func() *admin.MachineReport { return admin.Vitals(nd.Dir, netip.Addr{}) },
			Gateway: func() *admin.GatewayReport {
				return admin.GatewayFromDirty(nd.Ov.Dirty()).WithFlusher(nd.Flush, nd.Ov).WithReader(nd.Reader).WithFS(nd.FS)
			},
			Process:  admin.Process,
			IsLeader: nd.Node.IsLeader,
			Term:     nd.Node.Raft.CurrentTerm,
			Health:   func() admin.VolumeHealth { return admin.ScanHealth(nd.Node.FSM, nd.Prober, 10*time.Minute) },
			Stats:    nd.Node.FSM.Stats,
			Members:  nd.Node.FSM.Nodes,
			Live:     func(idx meta.NodeIdx) int { return stats.LiveState(nd.Prober.IsLive(idx), nd.Prober.IsSuspect(idx)) },
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harnessStats{svc: svc, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(h.done); svc.Run(ctx) }()
	e.stats[i] = h
}

func (e *harnessEnv) stopStats(i int) {
	if h := e.stats[i]; h != nil {
		h.cancel()
		<-h.done
		delete(e.stats, i)
	}
}

func (e *harnessEnv) stopAllStats() {
	for i := range e.stats {
		e.stopStats(i)
	}
}

func (e *harnessEnv) StatsSeal(ctx context.Context, n soak.NodeRef, rollup bool) (int, error) {
	e.mu.RLock()
	h := e.stats[e.node(n).I]
	e.mu.RUnlock()
	if h == nil {
		return 0, errors.New("node is down")
	}
	// the sampler samples on its own goroutine: a clock advance fires its timer and the
	// row lands once that goroutine runs, and a sampler (re)started just before the phase
	// makes its timer only after Recover, so an advance before that never fired it. A real
	// node has sampled by the time a second of wall time passes; here, a second of fake
	// time at a time until a row is in
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for h.svc.Writer.Unsealed() == 0 {
		e.c.Clk.Advance(time.Second)
		select {
		case <-tick.C:
		case <-deadline:
			return 0, errors.New("the sampler took no sample in 10 s")
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	rows := h.svc.Writer.Unsealed()
	if err := h.svc.Seal(ctx, rollup); err != nil {
		return 0, err
	}
	e.c.Settle()
	return rows, nil
}

func (e *harnessEnv) StatsLive(_ context.Context, n soak.NodeRef, since int64) ([]stats.Row, error) {
	e.mu.RLock()
	h := e.stats[e.node(n).I]
	e.mu.RUnlock()
	if h == nil {
		return nil, errors.New("node is down")
	}
	return h.svc.Live(since), nil
}

// WebURL serves node n's browser UI on a loopback listener for the phase, authorized as
// a member; a previous server for the node is closed first, since a restart rebuilt the
// node's handles.
func (e *harnessEnv) WebURL(_ context.Context, n soak.NodeRef) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return "", errors.New("node is down")
	}
	e.once.Do(func() {
		e.web, e.pins, e.stop = map[int]*httptest.Server{}, map[int]*pins.Pins{}, map[int]context.CancelFunc{}
		e.t.Cleanup(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			for _, stop := range e.stop {
				stop()
			}
			for _, srv := range e.web {
				srv.Close()
			}
		})
	})
	if srv := e.web[nd.I]; srv != nil {
		srv.Close()
	}
	if stop := e.stop[nd.I]; stop != nil {
		stop()
	}
	p, err := pins.Open(nd.Dir, nd.Node.FSM, nd.Reader, nd.Cache, e.c.Clk, nil)
	if err != nil {
		return "", err
	}
	wctx, cancel := context.WithCancel(context.Background())
	go p.Run(wctx)
	e.pins[nd.I], e.stop[nd.I] = p, cancel
	loop := netip.MustParseAddr("127.0.0.1")
	auth := &rpc.TableAuthorizer{Member: func(string) (meta.Node, bool) {
		return meta.Node{Idx: nd.Idx(), ID: nd.ID, Name: n.Name, Kind: meta.KindDevice, Addr: loop.String()}, true
	}}
	ui, err := webui.New(webui.Options{
		FS: nd.FS, Store: nd.Node.FSM, Auth: auth, Clk: e.c.Clk, Logger: slog.New(slog.DiscardHandler), Retention: 30 * 24 * time.Hour, Pins: p,
		Node:   n.Name, // an upload's temp is the node's own, as node.go wires it (I-226)
		Search: e.searchOf(nd), Catalog: e.searchOf(nd),
		Status: func() any { rep, _ := e.Status(context.Background(), n); return rep },
		Chunks: func(ctx context.Context, sel string, off, lim int) (int, []admin.ChunkReport, error) {
			return admin.DescribeChunksPage(ctx, nd.Node.FSM, nd.Prober, nd.Client, nd.Events, sel, off, lim)
		},
	})
	if err != nil {
		return "", err
	}
	srv := httptest.NewServer(ui.Handler())
	e.web[nd.I] = srv
	return srv.URL + "/", nil
}

func (e *harnessEnv) WebClient() *http.Client { return nil }

// bucketPrefix is where the harness's nodes keep their objects in the bucket.
const bucketPrefix = "soak"

// AddS3 registers the env's bucket as S3 node name with every node holding the key, as
// the secrets store does for `node add-s3` (docs/10 §4.1, docs/19 §6).
func (e *harnessEnv) AddS3(ctx context.Context, name string) error {
	if e.bucket == nil {
		return errors.New("harness: no bucket")
	}
	for i := range e.c.Nodes {
		e.c.SetS3Key(i, name, &s3test.Creds)
	}
	t := e.bucket.Target(bucketPrefix)
	sum := sha256.Sum256([]byte(name))
	op := rpc.MemberOp{Op: "add_s3", Name: name, Capacity: 1 << 40, S3: &rpc.S3Spec{
		ID: hex.EncodeToString(sum[:8]), Endpoint: t.Endpoint, Bucket: t.Bucket, Prefix: t.Prefix, PathStyle: t.PathStyle,
	}}
	if err := e.c.Leader().Node.MemberOp(ctx, op); err != nil {
		return err
	}
	e.c.Settle()
	e.c.Probe()
	return nil
}

func (e *harnessEnv) Bucket(context.Context) (*s3.Bucket, error) {
	if e.bucket == nil {
		return nil, errors.New("harness: no bucket")
	}
	return s3.New(e.bucket.Target(bucketPrefix), s3test.Creds, nil)
}

// The harness pins every budget at 1 TiB, so its fake disk is sized to derive exactly
// that: a 20 TiB filesystem (2 TiB reserve) with 3 TiB free minus what is held or filled.
const harnessDiskTotal = uint64(20) << 40

// Disks is the node's default disk (its Store) as its fake budget sees it; a diskless
// node has none (docs/14 §9).
func (e *harnessEnv) Disks(_ context.Context, n soak.NodeRef) ([]soak.DiskInfo, error) {
	nd := e.node(n)
	if nd.Store == nil {
		return nil, nil
	}
	e.mu.RLock()
	fill := e.fill[nd.I]
	e.mu.RUnlock()
	used := uint64(max(nd.Store.Used(), 0))
	row, _ := nd.Node.FSM.Node(nd.Target())
	return []soak.DiskInfo{{ID: row.ID, Name: row.Name, Used: used, Free: uint64(3)<<40 - used - fill, Total: harnessDiskTotal}}, nil
}

// Fill does on the fake disk what a scratch file and serve's next budget tick do on a real
// one: the free space moves, the store's cap follows, and a change past the hysteresis is
// registered through the leader.
func (e *harnessEnv) Fill(ctx context.Context, n soak.NodeRef, _ string, size uint64) error {
	nd := e.node(n)
	e.mu.Lock()
	if e.fill == nil {
		e.fill = map[int]uint64{}
	}
	e.fill[nd.I] = size
	e.mu.Unlock()
	ds, _ := e.Disks(ctx, n)
	if len(ds) == 0 {
		return nil
	}
	d := ds[0]
	b := chunk.Budget(d.Used, d.Free, d.Total)
	nd.Store.SetCapacity(int64(b))
	if cur, ok := nd.Node.FSM.Node(nd.Target()); ok && chunk.BudgetChanged(cur.Capacity, b) {
		return e.c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_capacity", NodeIdx: nd.Target(), Capacity: b})
	}
	return nil
}

func (e *harnessEnv) SetPolicyAuto(ctx context.Context) error {
	res, _, err := e.c.Leader().Node.Propose(ctx, meta.Command{Op: meta.OpSetVolumeMode, SetVolumeMode: &meta.SetVolumeMode{Auto: true, Now: e.c.Clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	return err
}

func (e *harnessEnv) Name() string { return "in-process harness" }

func (e *harnessEnv) Nodes(context.Context) ([]soak.NodeRef, error) {
	out := make([]soak.NodeRef, 0, len(e.c.Nodes))
	for _, nd := range e.c.Nodes {
		out = append(out, soak.NodeRef{Name: fmt.Sprintf("n%d", nd.I), Idx: nd.Idx(), RPC: nd.RPCAddr()})
	}
	return out, nil
}

// node finds the harness node behind a ref: device indices are not positions once disks
// share the index space (docs/14 §9), so the match is by index.
func (e *harnessEnv) node(n soak.NodeRef) *testharness.ClusterNode {
	for _, nd := range e.c.Nodes {
		if nd.Idx() == n.Idx {
			return nd
		}
	}
	e.t.Fatalf("soak: no harness node with index %d", n.Idx)
	return nil
}

func (e *harnessEnv) Volume(n soak.NodeRef) (soak.Volume, bool) {
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return nil, false
	}
	return soak.BillyVolume{FS: e.fsOf(n)}, true
}

func (e *harnessEnv) Caps() soak.Caps {
	return soak.Caps{Kill: true, Partition: true, Corrupt: true, MultiGateway: true, Disks: true, Mutate: true, S3: e.bucket != nil, AddS3: e.bucket != nil, Stats: e.stats != nil, Mirror: true}
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
	for d > 0 { // a second at a time, so the samplers' timers fire at every interval
		step := min(d, time.Second)
		e.c.Clk.Advance(step)
		d -= step
	}
	// the node handles below are what a restart replaces (phase 22 restarts a gateway on
	// a goroutine of its own while the phase keeps waiting)
	e.mu.RLock()
	defer e.mu.RUnlock()
	e.c.Probe()
	if !e.quorum() {
		return nil
	}
	// FlushAll skips an inode whose flush the background flusher already has under way
	// and returns before that commit exists, so flushing once can leave a commit out of
	// the log Settle waits on; other gateways then read the file at its create size (seen
	// once in about twenty -race runs, phase 2). Wait until no live node has a flush under
	// way, then flush: what is still dirty after that is what a cut-off holder keeps.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && e.flushing() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
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

// flushing reports whether a live node has a flush under way (the background flusher's).
func (e *harnessEnv) flushing() bool {
	for i := range e.c.Nodes {
		if !e.c.Alive(i) {
			continue
		}
		for _, d := range e.c.Nodes[i].Ov.Dirty() {
			if d.Flushing {
				return true
			}
		}
	}
	return false
}

func (e *harnessEnv) Status(_ context.Context, n soak.NodeRef) (admin.Report, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return admin.Report{}, errors.New("node is down")
	}
	rep := admin.Build(nd.Node, nd.Prober, e.c.CID, e.c.Clk, nil)
	rep.Gateway = admin.GatewayFromDirty(nd.Ov.Dirty()).WithFlusher(nd.Flush, nd.Ov).WithReader(nd.Reader)
	rep.Machine = admin.Vitals(nd.Dir, netip.Addr{})
	if p := e.pins[nd.I]; p != nil {
		c := p.List()
		rep.Cache = &c
	}
	rep.Mirrors = e.mirrorReports(nd.I)
	return rep, nil
}

func (e *harnessEnv) repairer(scrub bool) *repair.Repairer {
	cfg := repair.DefaultConfig()
	cfg.RateBytes = 0
	cfg.ScrubPeriod = 0
	if scrub {
		cfg.ScrubPeriod = time.Nanosecond // a fresh repairer's first pass then scrubs every chunk (I-215)
	}
	l := e.c.Leader()
	if !scrub && e.rep != nil && e.repNode == l.Node {
		return e.rep // the leader's repairer keeps its home gate across passes (docs/14 §7)
	}
	r := repair.New(l.Node.FSM, l.Node, l.Prober, l.Client, l.Reader, e.c.Clk, cfg, nil)
	r.Events = l.Events
	if !scrub {
		e.rep, e.repNode = r, l.Node
	}
	return r
}

func (e *harnessEnv) Rebalance(context.Context) (repair.HomePlan, error) {
	r := e.repairer(false)
	r.OpenHome()
	return r.PlanHome(), nil
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

func (e *harnessEnv) PurgeTrash(ctx context.Context) (int, error) {
	l := e.c.Leader()
	gc := repair.NewGC(l.Node.FSM, l.Node, l.Client, l.Prober, e.c.Clk, 10*time.Minute, nil)
	n, err := gc.PurgeTrash(ctx, max(e.c.Clk.Now().UnixNano(), l.Node.FSM.LastNow())+1)
	e.c.Settle()
	return n, err
}

// SetPolicy pins a policy as the admin op does: manual mode first, then the policy.
func (e *harnessEnv) SetPolicy(ctx context.Context, k, m uint8) error {
	l := e.c.Leader()
	now := e.c.Clk.Now().UnixNano()
	cmds := []meta.Command{
		{Op: meta.OpSetVolumeMode, SetVolumeMode: &meta.SetVolumeMode{Auto: false, Now: now}},
		{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: k, M: m, Now: now}},
	}
	for i := range cmds {
		res, _, err := l.Node.Propose(ctx, cmds[i])
		if err == nil {
			err = res.Err
		}
		if err != nil {
			e.c.Settle()
			return err
		}
	}
	e.c.Settle()
	return nil
}

func (e *harnessEnv) SetState(ctx context.Context, n soak.NodeRef, st meta.NodeState) error {
	err := e.c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: n.Idx, State: st})
	e.c.Settle()
	return err
}

func (e *harnessEnv) Fsck(_ context.Context, n soak.NodeRef) (soak.FsckResult, error) {
	nd := e.node(n)
	if !e.c.Alive(nd.I) {
		return soak.FsckResult{}, errors.New("node is down")
	}
	snap, err := nd.Node.FSM.Snapshot()
	if err != nil {
		return soak.FsckResult{}, err
	}
	st := snap.(interface{ State() *meta.State }).State()
	v := meta.CheckInvariants(st)
	// orphans as the node's fsck counts and lists them: shards on its disk the chunk table
	// does not place there (a diskless node has none, docs/14 §9)
	var rep admin.FsckReport
	if nd.Store != nil {
		_ = nd.Store.Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error {
			h, _, _, idx, err := chunk.ParseKey(k)
			if err != nil {
				rep.AddOrphan(string(k))
				return nil
			}
			c, ok := st.Chunks[h]
			if !ok || idx >= len(c.Placement) || c.Placement[idx] != nd.Target() {
				rep.AddOrphan(string(k))
			}
			return nil
		})
	}
	return soak.FsckResult{OK: len(v) == 0, Orphans: rep.OrphanShards, Listed: rep.Orphans, Detail: strings.Join(v, "; ")}, nil
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
	e.stopStats(e.node(n).I)
	e.stopSMB(n)
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
	if e.stats != nil {
		e.startStats(e.node(n).I)
	}
	return nil
}

func (e *harnessEnv) Restart(ctx context.Context, nodes []soak.NodeRef) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range nodes {
		e.stopStats(e.node(n).I)
		e.stopSMB(n)
		e.c.Kill(e.node(n).I)
	}
	for _, n := range nodes {
		e.c.Restart(e.node(n).I)
	}
	e.c.Settle()
	if e.stats != nil {
		for _, n := range nodes {
			e.startStats(e.node(n).I)
		}
	}
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

// Corrupt flips a byte of every copy of the shard on the host's disks, as /dev/corrupt
// does: a rebalance within the host (I-172) leaves the old copy on its disk until the
// sweep, and phase 5's scrub found nothing when only that orphan was corrupted.
func (e *harnessEnv) Corrupt(_ context.Context, n soak.NodeRef, key string) error {
	nd := e.node(n)
	k := chunk.ShardKey(key)
	stores := []*chunk.Store{nd.Store}
	nodes := nd.Node.FSM.Nodes()
	for i := range nodes { // the host's disks; its default disk is nd.Store again, and a
		// second flip of the same file would restore it
		if t := &nodes[i]; t.Kind == meta.KindDisk && t.Host == nd.Idx() {
			if s := e.c.DiskStore(nd.I, t.Idx); s != nil && !slices.Contains(stores, s) {
				stores = append(stores, s)
			}
		}
	}
	copies := 0
	for _, s := range stores {
		if !s.Has(k) {
			continue
		}
		if err := dev.Corrupt(s.Path(k)); err != nil {
			return err
		}
		copies++
	}
	if copies == 0 {
		return errors.New("no such shard")
	}
	return nil
}

// hostOf finds the harness node serving a disk ref by its host name.
func (e *harnessEnv) hostOf(disk soak.NodeRef) (int, error) {
	for i := range e.c.Nodes {
		if fmt.Sprintf("n%d", i) == disk.Host {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no host %q", disk.Host)
}

func (e *harnessEnv) AddDisk(_ context.Context, host soak.NodeRef) error {
	e.c.AddDisk(e.node(host).I)
	return nil
}

func (e *harnessEnv) Remove(ctx context.Context, n soak.NodeRef) error {
	if err := e.c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: n.Idx}); err != nil {
		return err
	}
	e.c.Settle()
	return nil
}

func (e *harnessEnv) FailDisk(_ context.Context, disk soak.NodeRef) error {
	i, err := e.hostOf(disk)
	if err != nil {
		return err
	}
	e.c.FailDisk(i, disk.Idx, true)
	e.c.Probe()
	return nil
}

func (e *harnessEnv) RestoreDisk(_ context.Context, disk soak.NodeRef) error {
	i, err := e.hostOf(disk)
	if err != nil {
		return err
	}
	e.c.FailDisk(i, disk.Idx, false)
	e.c.Probe()
	return nil
}

func harnessProfile() soak.Profile {
	cs := int(testharness.ChunkSize)
	return soak.Profile{
		Name: "harness", BigFile: 3*cs + 1000, LargeFile: 2 << 20, SmallFiles: 8, ChurnOps: 40, LeaderKills: 1, ChunkSize: cs,
		FlushIdle: time.Second, FillAfter: cluster.FillAfter + time.Second, Grace: 10*time.Minute + time.Second, RestartSettle: time.Second,
		SampleEvery: 50 * time.Millisecond,
	} // the sampler runs concurrently with the phases, as it does in production
}

// The whole scenario sequence runs green against the in-process cluster, every phase
// reports checks (none skipped), the results stream has samples and events, and the
// report renders with the phase matrix and charts. The partition phase is the only
// end-to-end exercise of a cut-off gateway: metadata refused, WRITEs buffered and
// committed after the heal, the majority committing throughout.
// Invariant: I-29
// Lifecycle: L-59, L-148
func TestSoak_AllPhasesAgainstHarness(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1, testharness.WALCap(512<<10)) // the large-file phase meets the cap
	defer c.Close()
	for i := range c.Nodes { // 4 hosts × 3 targets plus a diskless voter, as the compose rig (docs/14)
		c.AddDisk(i)
		c.AddDisk(i)
	}
	c.AddDisklessNode(true)
	bucket := s3test.New() // phase 15's bucket, as the compose rig's s3 service
	defer bucket.Close()
	env := &harnessEnv{t: t, c: c, bucket: bucket}
	for i := range c.Nodes {
		env.startStats(i)
	}
	defer env.stopAllStats()
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	// phases 22 and 23 need the gateways' retry-until-stale (production's) and run in
	// tests of their own on such a cluster; this one's gateways fail fast for the other
	// fault phases
	var all []string
	for _, ph := range soak.StandardPhases() {
		if num, _, _ := strings.Cut(ph.Name, " "); num != "22" && num != "23" {
			all = append(all, num)
		}
	}
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: soak.StandardPhases(all...)}
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
		if ph.Name == "15 s3 bucket" || ph.Name == "16 web ui" || ph.Name == "19 snapshots" || ph.Name == "21 search and media" || ph.Name == "24 secrets" { // the harness offers everything these phases can use
			for _, chk := range ph.Checks {
				if chk.OK == nil {
					t.Errorf("%s: %s skipped: %s", ph.Name, chk.Name, chk.Text)
				}
			}
		}
	}
	if sum.Failed != 0 || len(sum.Phases) != 22 {
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
	for _, want := range []string{
		"<svg", "9 restart storm", "10 partition", ">PASS —", "FSM-hash agreement", "Throughput", fmt.Sprintf("seed <code>%d</code>", seed),
		"policy 2&#43;1, chunk", "5 nodes", "Node liveness", "under faults", "I-12", "L-59", "Overlay WAL depth", "refused metadata operation",
		"12 large file", "Process memory", "peak heap in use", "flush buffers stayed within the bound",
		"CPU on the sampled node", "Memory on the sampled node", "Data disk on the sampled node", "peak resident set",
		"Large file: shard slots per node", "data slots of the large file spread among voters by capacity",
		"13 disks", "Data spread across disks", "every chunk within the host cap", "the closed disk is dead while its host is live",
		"policy follows the member table after a disk was added", "the drained disk holds no slots", "the removed disk is out of the node table",
		"14 diskless host", "status reports the host diskless with no budget", "no slot names the diskless host or a disk of it",
		"15 s3 bucket", "every slot on the bucket is its shard, in the disk layout and framing", "a read right after the holder died falls through to the bucket", "a read with a data holder down is served from the bucket",
		"the scrub finds the missing object and repair puts it back", "gc collects a file on the bucket without deleting its objects",
		"the drained bucket is out of the node table",
		"19 snapshots", "Snapshot round trip", "bytes on the disk (mirrors[].used)", "What each snapshot added to its mirror", "catch-up on attach", "every file of soak-",
	} {
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
	if sum.Partial || sum.K != 2 || sum.M != 1 || sum.NodeCount != 5 || sum.Totals.BytesWritten == 0 || sum.Totals.Repair.Scanned == 0 || sum.Totals.GC.Victims == 0 || len(sum.Coverage) < 30 {
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
