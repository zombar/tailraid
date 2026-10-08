package testharness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/vfs"
)

// FaultNet is an http.RoundTripper that drops or delays requests per destination
// address, so the production RPC client and server are exercised under partitions.
type FaultNet struct {
	base http.RoundTripper
	mu   sync.Mutex
	cut  map[string]bool // destination host:port → unreachable
	all  bool
}

var errCut = errors.New("faultnet: link cut")

// NewFaultNet wraps a transport.
func NewFaultNet(base http.RoundTripper) *FaultNet {
	return &FaultNet{base: base, cut: map[string]bool{}}
}

func (f *FaultNet) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	cut := f.all || f.cut[r.URL.Host]
	f.mu.Unlock()
	if cut {
		return nil, errCut
	}
	return f.base.RoundTrip(r)
}

// Cut makes dst unreachable from this client; Heal restores it.
func (f *FaultNet) Cut(dst string, cut bool) {
	f.mu.Lock()
	f.cut[dst] = cut
	f.mu.Unlock()
}

// Isolate cuts every destination.
func (f *FaultNet) Isolate(on bool) {
	f.mu.Lock()
	f.all = on
	f.mu.Unlock()
}

// ClusterNode is one in-process node: real Raft (in-memory transport), real shard
// store, overlay, flusher, reader, filesystem and RPC server on a loopback port.
type ClusterNode struct {
	I      int
	ID     string
	Dir    string
	Node   *cluster.Node
	Store  *chunk.Store
	Cache  *chunk.Cache
	Ov     *vfs.Overlay
	Prober *cluster.Prober
	Client *rpc.Client
	Net    *FaultNet
	FS     *vfs.FS
	Flush  *vfs.Flusher
	Reader *vfs.Reader
	Events *meta.EventRing // chunk history (always on in the harness)
	RPC    *rpc.Server
	srv    *httptest.Server
	port   int
	trans  *raft.InmemTransport
	raddr  raft.ServerAddress
	alive  bool // guarded by Cluster.mu
	// Raft state lives in memory and survives a simulated kill, like the page cache of a
	// process kill would; production uses the fsynced bbolt store (docs/04 §8.1)
	stores  *cluster.Stores
	started bool
}

// RaftTiming is the harness's shortened Raft timing: quick elections, but a lease long
// enough that a leader under the race detector's load does not lose it to a slow
// follower (CI showed 25 ms contact times stepping a 25 ms lease into permanent churn).
func RaftTiming() *cluster.RaftTiming {
	return &cluster.RaftTiming{Heartbeat: time.Second, Election: time.Second, LeaderLease: 500 * time.Millisecond,
		Commit: 10 * time.Millisecond, SnapshotInterval: 500 * time.Millisecond}
}

// Cluster is an in-process multi-node tailraid.
type Cluster struct {
	T     *testing.T
	Clk   *clock.Fake
	Nodes []*ClusterNode
	K, M  uint8
	CID   [16]byte
	mu    sync.Mutex
	snap  uint64

	retryUntilStale bool
}

// Idx is the node's FSM index (1-based, same as creation order).
func (n *ClusterNode) Idx() meta.NodeIdx { return meta.NodeIdx(n.I + 1) }

const harnessStableID = "harness"

// inmem transport addresses by raft ID, so RaftAddrFor resolves records to them
var (
	inmemMu    sync.Mutex
	inmemAddrs = map[string]raft.ServerAddress{}
)

func init() {
	cluster.RaftAddrFor = func(nd meta.Node) raft.ServerAddress {
		inmemMu.Lock()
		defer inmemMu.Unlock()
		if a, ok := inmemAddrs[nd.ID]; ok {
			return a
		}
		return raft.ServerAddress(net.JoinHostPort(nd.Addr, strconv.Itoa(int(nd.RaftPort))))
	}
}

// ClusterOption tunes NewCluster.
type ClusterOption func(*clusterOpts)

type clusterOpts struct {
	snapshotThreshold uint64
	chunkSize         uint32
	retryUntilStale   bool
}

// RetryUntilStale gives every node's filesystem the production health view: a failed
// metadata proposal is retried on the fake clock until the node is stale (L-187). Tests
// that use it must drive c.Clk while a mutation is in flight; without it a failed
// proposal is EROFS at once, so fault tests fail fast.
func RetryUntilStale() ClusterOption {
	return func(o *clusterOpts) { o.retryUntilStale = true }
}

// SnapshotThreshold lowers Raft's snapshot threshold (tests of snapshot install).
func SnapshotThreshold(n uint64) ClusterOption {
	return func(o *clusterOpts) { o.snapshotThreshold = n }
}

// NewCluster bootstraps node 0 and joins n-1 more through the real RPC join; all are
// voters. Returns once every node sees the same leader and membership.
func NewCluster(t *testing.T, n int, k, m uint8, opts ...ClusterOption) *Cluster {
	t.Helper()
	o := clusterOpts{chunkSize: ChunkSize}
	for _, f := range opts {
		f(&o)
	}
	c := &Cluster{T: t, Clk: clock.NewFake(time.Now()), K: k, M: m, snap: o.snapshotThreshold, retryUntilStale: o.retryUntilStale}
	for i := 0; i < n; i++ {
		c.Nodes = append(c.Nodes, c.newNode(i))
	}
	first := c.Nodes[0]
	first.start(c, true, meta.SetVolume{ChunkSize: o.chunkSize, K: k, M: m, Now: 1})
	for _, nd := range c.Nodes[1:] {
		nd.start(c, false, meta.SetVolume{})
		c.join(nd)
	}
	c.Settle()
	return c
}

func (c *Cluster) newNode(i int) *ClusterNode {
	c.T.Helper()
	dir := filepath.Join(c.T.TempDir(), fmt.Sprintf("n%d", i))
	id, err := cluster.LoadOrCreateNodeID(dir)
	if err != nil {
		c.T.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.T.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	nd := &ClusterNode{I: i, ID: id, Dir: dir, port: port}
	nd.raddr, nd.trans = raft.NewInmemTransportWithTimeout(raft.ServerAddress(id), 10*time.Second)
	inmemMu.Lock()
	inmemAddrs[id] = nd.raddr
	inmemMu.Unlock()
	return nd
}

func (nd *ClusterNode) record(c *Cluster) meta.Node {
	return meta.Node{ID: nd.ID, Name: fmt.Sprintf("n%d", nd.I), Kind: meta.KindDevice, TSStableID: harnessStableID, Addr: "127.0.0.1",
		RaftPort: 1, RPCPort: uint16(nd.port), Voter: true, Capacity: 1 << 40}
}

// start opens stores and raft (bootstrap or fresh/reopen) and brings up the stack.
func (nd *ClusterNode) start(c *Cluster, bootstrap bool, vol meta.SetVolume) {
	c.T.Helper()
	// a restarted node needs a fresh in-memory transport at the same address
	if nd.trans == nil {
		nd.raddr, nd.trans = raft.NewInmemTransportWithTimeout(nd.raddr, 10*time.Second)
	}
	c.connectAll()
	if nd.stores == nil {
		st := raft.NewInmemStore()
		nd.stores = &cluster.Stores{Logs: st, Stable: st, Snaps: raft.NewInmemSnapshotStore()}
	}
	opts := cluster.Options{DataDir: nd.Dir, NodeID: nd.ID, ClusterID: c.CID, Transport: nd.trans, Timing: RaftTiming(), Stores: nd.stores, SnapshotThreshold: c.snap}
	if os.Getenv("TAILRAID_HARNESS_DEBUG") != "" {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("node", fmt.Sprintf("n%d", nd.I))
	}
	var err error
	switch {
	case bootstrap:
		nd.Node, err = cluster.Bootstrap(context.Background(), opts, nd.record(c), vol, "harness-token")
	case nd.started:
		nd.Node, err = cluster.Open(opts)
	default:
		nd.Node, err = cluster.OpenFresh(opts)
	}
	if err != nil {
		c.T.Fatal(err)
	}
	nd.started = true
	if bootstrap {
		if c.CID, err = cluster.LoadClusterID(nd.Dir); err != nil {
			c.T.Fatal(err)
		}
	}
	nd.Store, err = chunk.Open(sysfs.OS{}, filepath.Join(nd.Dir, "shards"), 1<<40)
	if err != nil {
		c.T.Fatal(err)
	}
	nd.Cache, err = chunk.OpenCache(sysfs.OS{}, filepath.Join(nd.Dir, "cache"), 1<<30, c.Clk)
	if err != nil {
		c.T.Fatal(err)
	}
	cs := ChunkSize
	if v := nd.Node.FSM.Volume(); v.ChunkSize != 0 {
		cs = v.ChunkSize
	}
	nd.Ov, err = vfs.OpenOverlay(filepath.Join(nd.Dir, "staging"), cs, c.Clk)
	if err != nil {
		c.T.Fatal(err)
	}
	nd.Net = NewFaultNet(http.DefaultTransport)
	nd.Client = rpc.NewClient(c.CID, nd.Node.RPCAddrOf, nd.Store)
	nd.Client.SetTransport(nd.Net)
	nd.Node.SetForwarder(nd.Client)
	nd.Prober = cluster.NewProber(nd.Node, nd.Client, c.Clk)
	nd.Events = meta.NewEventRing(c.Clk, nd.Idx())
	nd.Reader = vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nd.Cache, nil)
	nd.Reader.Events = nd.Events
	cfg := vfs.DefaultConfig()
	cfg.PutDeadline = 2 * time.Second
	nd.Flush = vfs.NewFlusher(nd.Ov, nd.Node.FSM, nd.Node, nd.Prober, nd.Client, nd.Reader, nd.Cache, c.Clk, cfg, nil)
	nd.Flush.Events = nd.Events
	nd.FS = vfs.New(nd.Node.FSM, nd.Node, nd.Ov, nd.Reader, c.Clk, 501, 20)
	if c.retryUntilStale {
		nd.FS.SetHealth(func() bool {
			switch h := nd.Node.Health(c.Clk, nd.Prober); h.State {
			case cluster.Stale:
				return true
			case cluster.NoLeader:
				return c.Clk.Now().Sub(nd.Node.Raft.LastContact()) >= cluster.StaleThreshold
			}
			return false
		})
	}
	nd.RPC, err = rpc.NewServer(rpc.Options{NodeID: nd.ID, ClusterID: c.CID, Version: "harness", Store: nd.Store, Applier: nd.Node,
		Fence: fence{nd}, Joiner: nd.Node, Auth: rpc.AllowAll{Node: meta.Node{TSStableID: harnessStableID}}, Members: nd.Node.FSM.Nodes,
		JoinToken: func() string { return nd.Node.FSM.Meta().JoinToken }, Clk: c.Clk, MaxShard: 1 << 20, Grace: 10 * time.Minute})
	if err != nil {
		c.T.Fatal(err)
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", nd.port))
	if err != nil {
		c.T.Fatal(err)
	}
	nd.srv = &httptest.Server{Listener: l, Config: &http.Server{Handler: nd.RPC.Handler(), ReadHeaderTimeout: 5 * time.Second}}
	nd.srv.Start()
	c.setAlive(nd, true)
}

type fence struct{ nd *ClusterNode }

func (f fence) PlacedOnSelf(key chunk.ShardKey) (bool, bool) {
	h, k, m, idx, err := chunk.ParseKey(key)
	if err != nil {
		return false, false
	}
	c, ok := f.nd.Node.FSM.GetChunk(h)
	if !ok || c.K != k || c.M != m || idx >= len(c.Placement) {
		return false, ok
	}
	self, _ := f.nd.Node.SelfIdx()
	return c.Placement[idx] == self, true
}

// connectAll links every live in-memory raft transport pairwise.
func (c *Cluster) connectAll() {
	for _, a := range c.Nodes {
		if a.trans == nil {
			continue
		}
		for _, b := range c.Nodes {
			if b.trans == nil || a == b {
				continue
			}
			a.trans.Connect(b.raddr, b.trans)
		}
	}
}

func (c *Cluster) join(nd *ClusterNode) {
	c.T.Helper()
	leader := c.Leader()
	req := rpc.JoinRequest{Token: "harness-token", NodeID: nd.ID, Name: fmt.Sprintf("n%d", nd.I), TSStableID: harnessStableID,
		Addr: "127.0.0.1", RaftPort: 1, RPCPort: uint16(nd.port), Voter: true, Capacity: 1 << 40, Version: "harness"}
	var last error
	for attempt := 0; attempt < 50; attempt++ {
		_, last = nd.Client.Join(context.Background(), leader.RPCAddr(), req)
		if last == nil {
			return
		}
		if !errors.Is(last, rpc.ErrNotLeader) && !errors.Is(last, rpc.ErrNoLeader) && !errors.Is(last, rpc.ErrUnavailable) {
			break
		}
		<-time.After(50 * time.Millisecond)
		leader = c.Leader()
	}
	c.T.Fatalf("join n%d: %v", nd.I, last)
}

// ChunksOf returns the committed chunk entries of path as seen by node i.
func (c *Cluster) ChunksOf(i int, path string) []meta.Chunk {
	c.T.Helper()
	nd := c.Nodes[i]
	in, err := nd.Node.FSM.Resolve(splitPath(path))
	if err != nil {
		c.T.Fatalf("resolve %s: %v", path, err)
	}
	var out []meta.Chunk
	for _, h := range in.Chunks {
		if h == meta.HashZero {
			continue
		}
		if ch, ok := nd.Node.FSM.GetChunk(h); ok {
			out = append(out, ch)
		}
	}
	return out
}

func splitPath(p string) []string {
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return parts
}

// ReadFileCold reads path on node i through a reader with no cache, so every chunk
// is reconstructed from shards.
func (c *Cluster) ReadFileCold(i int, path string) ([]byte, error) {
	nd := c.Nodes[i]
	in, err := nd.Node.FSM.Resolve(splitPath(path))
	if err != nil {
		return nil, err
	}
	rd := vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil)
	buf := make([]byte, in.Size)
	n, err := rd.ReadAt(context.Background(), in, buf, 0)
	if err != nil && n != len(buf) {
		return buf[:n], err
	}
	return buf[:n], nil
}

// RPCAddr is the node's RPC address.
func (nd *ClusterNode) RPCAddr() string { return fmt.Sprintf("127.0.0.1:%d", nd.port) }

// Leader waits for and returns the current leader.
func (c *Cluster) Leader() *ClusterNode {
	c.T.Helper()
	var leader *ClusterNode
	Eventually(c.T, func() bool {
		for _, nd := range c.Nodes {
			if c.isAlive(nd) && nd.Node.IsLeader() {
				leader = nd
				return true
			}
		}
		return false
	}, "leader election")
	return leader
}

// Settle waits until every live node has applied the leader's last index and refreshes
// liveness on every prober.
func (c *Cluster) Settle() {
	c.T.Helper()
	// a barrier can fail when leadership moves under load; re-find the leader and retry
	var leader *ClusterNode
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		leader = c.Leader()
		if err = leader.Node.Raft.Barrier(5 * time.Second).Error(); err == nil {
			break
		}
		<-time.After(100 * time.Millisecond)
	}
	if err != nil {
		c.T.Fatalf("settle: %v", err)
	}
	last := leader.Node.Raft.LastIndex()
	inConfig := map[string]bool{}
	if cfg := leader.Node.Raft.GetConfiguration(); cfg.Error() == nil {
		for _, s := range cfg.Configuration().Servers {
			inConfig[string(s.ID)] = true
		}
	}
	lagging := func() string {
		var b strings.Builder
		for _, nd := range c.Nodes {
			if c.isAlive(nd) {
				fmt.Fprintf(&b, " n%d=%d/%d(%s)", nd.I, nd.Node.Raft.AppliedIndex(), nd.Node.Raft.LastIndex(), nd.Node.Raft.State())
			}
		}
		return b.String()
	}
	Eventually(c.T, func() bool {
		for _, nd := range c.Nodes {
			if c.isAlive(nd) && inConfig[nd.ID] && nd.Node.Raft.AppliedIndex() < last {
				return false
			}
		}
		return true
	}, "followers catch up to "+strconv.FormatUint(last, 10)+":"+lagging())
	for _, nd := range c.Nodes {
		if c.isAlive(nd) && inConfig[nd.ID] {
			if err := nd.Node.WaitReady(context.Background(), 5*time.Second); err != nil {
				c.T.Fatal(err)
			}
		}
	}
	c.Probe()
}

// Probe runs one probe round on every live node.
func (c *Cluster) Probe() {
	for _, nd := range c.Nodes {
		if c.isAlive(nd) {
			nd.Prober.Tick(context.Background())
		}
	}
}

// Kill stops a node: raft, RPC and overlay are closed; the data directory is kept.
func (c *Cluster) Kill(i int) {
	c.T.Helper()
	nd := c.Nodes[i]
	if !c.isAlive(nd) {
		return
	}
	c.setAlive(nd, false)
	nd.srv.Close()
	nd.trans.DisconnectAll()
	for _, other := range c.Nodes {
		if other != nd && other.trans != nil {
			other.trans.Disconnect(nd.raddr)
		}
	}
	_ = nd.Ov.Close()
	if err := nd.Node.Close(); err != nil {
		c.T.Logf("close n%d: %v", i, err)
	}
	_ = nd.trans.Close()
	nd.trans = nil
	for _, other := range c.Nodes {
		if c.isAlive(other) {
			other.Net.Cut(nd.RPCAddr(), true)
		}
	}
}

// Restart reopens a killed node from its data directory.
func (c *Cluster) Restart(i int) {
	c.T.Helper()
	nd := c.Nodes[i]
	if c.isAlive(nd) {
		return
	}
	nd.start(c, false, meta.SetVolume{})
	for _, other := range c.Nodes {
		if c.isAlive(other) {
			other.Net.Cut(nd.RPCAddr(), false)
		}
	}
}

// Partition splits the cluster into two sides (raft and RPC links cut across sides).
func (c *Cluster) Partition(a, b []int) {
	in := func(set []int, i int) bool {
		for _, x := range set {
			if x == i {
				return true
			}
		}
		return false
	}
	for _, x := range a {
		for _, y := range b {
			nx, ny := c.Nodes[x], c.Nodes[y]
			if nx.trans != nil && ny.trans != nil {
				nx.trans.Disconnect(ny.raddr)
				ny.trans.Disconnect(nx.raddr)
			}
			if c.isAlive(nx) {
				nx.Net.Cut(ny.RPCAddr(), true)
			}
			if c.isAlive(ny) {
				ny.Net.Cut(nx.RPCAddr(), true)
			}
		}
	}
	_ = in
}

// Heal reconnects every live node to every other.
func (c *Cluster) Heal() {
	c.connectAll()
	for _, a := range c.Nodes {
		if !c.isAlive(a) {
			continue
		}
		for _, b := range c.Nodes {
			if c.isAlive(b) {
				a.Net.Cut(b.RPCAddr(), false)
			}
		}
	}
}

// Close shuts every node down.
func (c *Cluster) Close() {
	for i := range c.Nodes {
		c.Kill(i)
	}
}

// Alive reports whether node i is running.
func (c *Cluster) Alive(i int) bool { return c.isAlive(c.Nodes[i]) }

func (c *Cluster) isAlive(nd *ClusterNode) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return nd.alive
}

func (c *Cluster) setAlive(nd *ClusterNode, v bool) {
	c.mu.Lock()
	nd.alive = v
	c.mu.Unlock()
}

// WriteFile writes data at path through node i's filesystem and flushes it.
func (c *Cluster) WriteFile(i int, path string, data []byte) {
	c.T.Helper()
	nd := c.Nodes[i]
	f, err := nd.FS.Create(path)
	if err != nil {
		c.T.Fatalf("n%d create %s: %v", i, path, err)
	}
	if _, err := f.Write(data); err != nil {
		c.T.Fatalf("n%d write %s: %v", i, path, err)
	}
	_ = f.Close()
	for attempt := 0; attempt < 30; attempt++ { // a flush fails across a leadership change and is retried
		if err = nd.Flush.FlushAll(context.Background()); err == nil {
			return
		}
		<-time.After(100 * time.Millisecond)
	}
	c.T.Fatalf("n%d flush: %v", i, err)
}

// ReadFile reads path through node i's filesystem with a cold cache.
func (c *Cluster) ReadFile(i int, path string) ([]byte, error) {
	nd := c.Nodes[i]
	st, err := nd.FS.Stat(path)
	if err != nil {
		return nil, err
	}
	f, err := nd.FS.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, st.Size())
	n, err := f.ReadAt(buf, 0)
	if err != nil && n != len(buf) {
		return buf[:n], err
	}
	return buf[:n], nil
}
