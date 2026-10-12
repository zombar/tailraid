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
	"slices"
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
	base     http.RoundTripper
	mu       sync.Mutex
	cut      map[string]bool // destination host:port → unreachable
	all      bool
	putDelay time.Duration // every shard PUT waits this long first (soak phase 22)
}

// DelayPuts holds every shard PUT this client sends for d first (0: none), on the wall
// clock: the flush falls behind its writers as on a slow link.
func (f *FaultNet) DelayPuts(d time.Duration) {
	f.mu.Lock()
	f.putDelay = d
	f.mu.Unlock()
}

var errCut = errors.New("faultnet: link cut")

// NewFaultNet wraps a transport.
func NewFaultNet(base http.RoundTripper) *FaultNet {
	return &FaultNet{base: base, cut: map[string]bool{}}
}

func (f *FaultNet) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	cut := f.all || f.cut[r.URL.Host]
	delay := f.putDelay
	f.mu.Unlock()
	if cut {
		// a cut link drops the request before it leaves, as a refused dial does: the
		// forwarder retries it like a missing leader rather than resolving an unknown
		// outcome (I-95), the same as dev.Faults on the compose rig
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errCut}
	}
	if delay > 0 && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/shards/") {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return nil, r.Context().Err()
		}
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
	first  net.Listener // the RPC port, held from allocation until the first start so no other process can take it
	trans  *raft.InmemTransport
	raddr  raft.ServerAddress
	alive  bool // guarded by Cluster.mu
	// Raft state lives in memory and survives a simulated kill, like the page cache of a
	// process kill would; production uses the fsynced bbolt store (docs/04 §8.1)
	stores  *cluster.Stores
	started bool
	disks   nodeDisks    // the node's disks (docs/14), kept across restarts; the first is its Store
	idx     meta.NodeIdx // the device row's index, recorded at bootstrap or join
	s3      nodeS3Keys   // this node's S3 credentials (docs/10 §4.1), kept across restarts
	// lifecycle serialises Kill and Restart of this node: Kill marks the node dead before
	// it has torn it down, and a test that restarts it on seeing it dead (one killed from
	// a flusher hook on another goroutine) must not start it under the teardown
	lifecycle sync.Mutex
}

// RaftTiming is the harness's shortened Raft timing: quick elections, but a lease long
// enough that a leader under the race detector's load does not lose it to a slow
// follower (CI showed 25 ms contact times stepping a 25 ms lease into permanent churn).
func RaftTiming() *cluster.RaftTiming {
	return &cluster.RaftTiming{
		Heartbeat: time.Second, Election: time.Second, LeaderLease: 500 * time.Millisecond,
		Commit: 10 * time.Millisecond, SnapshotInterval: 500 * time.Millisecond,
	}
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
	nonVoters       map[int]bool
	auto            bool
	walCap          int64        // per-inode WAL cap for every gateway (0 = default)
	diskless        map[int]bool // nodes that get no default disk (docs/14 §9)
}

// Voter reports whether node i joins as a Raft voter (all do unless NonVoters says otherwise).
func (c *Cluster) Voter(i int) bool { return !c.nonVoters[i] }

// AddNode creates one more node after NewCluster, joins it through the leader (as a voter
// or not) and settles. Returns its index.
func (c *Cluster) AddNode(voter bool) int {
	c.T.Helper()
	i := len(c.Nodes)
	if !voter {
		if c.nonVoters == nil {
			c.nonVoters = map[int]bool{}
		}
		c.nonVoters[i] = true
	}
	nd := c.newNode(i)
	c.Nodes = append(c.Nodes, nd)
	nd.start(c, false, meta.SetVolume{})
	c.join(nd)
	c.Settle()
	if !c.diskless[i] {
		c.AddDisk(i)
	}
	return i
}

// AddDisklessNode joins a node that gets no disk: a gateway-only host (docs/14 §9).
func (c *Cluster) AddDisklessNode(voter bool) int {
	c.T.Helper()
	if c.diskless == nil {
		c.diskless = map[int]bool{}
	}
	c.diskless[len(c.Nodes)] = true
	return c.AddNode(voter)
}

// Idx is the node's FSM index: the device row, which is a Raft member, never a target.
// Device rows and disk rows share one index space, so this is only the creation order
// while no disk has been added; the harness records it at join.
func (n *ClusterNode) Idx() meta.NodeIdx { return n.idx }

// Target is the index of the node's default disk (its Store), 0 for a diskless node.
func (n *ClusterNode) Target() meta.NodeIdx {
	n.disks.mu.Lock()
	defer n.disks.mu.Unlock()
	if len(n.disks.disks) == 0 {
		return 0
	}
	return n.disks.disks[0].idx
}

const harnessStableID = "harness"

// inmem transport addresses by raft ID; cluster.Options.RaftAddrFor resolves records to
// them (every node of a harness cluster shares the map)
var (
	inmemMu    sync.Mutex
	inmemAddrs = map[string]raft.ServerAddress{}
)

func inmemAddrFor(nd meta.Node) raft.ServerAddress {
	inmemMu.Lock()
	defer inmemMu.Unlock()
	if a, ok := inmemAddrs[nd.ID]; ok {
		return a
	}
	return raft.ServerAddress(nd.RaftAddr())
}

// ClusterOption tunes NewCluster.
type ClusterOption func(*clusterOpts)

type clusterOpts struct {
	snapshotThreshold uint64
	chunkSize         uint32
	retryUntilStale   bool
	nonVoters         map[int]bool
	auto              bool
	walCap            int64
	diskless          map[int]bool
}

// WALCap sets every gateway's per-inode WAL cap, so a run with small files still meets
// the cap and the wait behind it (docs/04 §3).
func WALCap(n int64) ClusterOption {
	return func(o *clusterOpts) { o.walCap = n }
}

// AutoPolicy bootstraps the volume in automatic redundancy mode (docs/03): the leader's
// repair passes raise and lower the policy as members come and go.
func AutoPolicy() ClusterOption {
	return func(o *clusterOpts) { o.auto = true }
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

// NonVoters makes the listed nodes (by creation index) join as Raft non-voters: the
// laptops of docs/00, which placement keeps off the data slots (docs/03).
func NonVoters(idx ...int) ClusterOption {
	return func(o *clusterOpts) {
		if o.nonVoters == nil {
			o.nonVoters = map[int]bool{}
		}
		for _, i := range idx {
			o.nonVoters[i] = true
		}
	}
}

// Diskless makes the listed nodes (by creation index) hosts with no disk: members and
// gateways that store nothing (docs/14 §9). Every other node gets one default disk,
// n<i>/1, whose store is its Store.
func Diskless(idx ...int) ClusterOption {
	return func(o *clusterOpts) {
		if o.diskless == nil {
			o.diskless = map[int]bool{}
		}
		for _, i := range idx {
			o.diskless[i] = true
		}
	}
}

// NewCluster bootstraps node 0 and joins n-1 more through the real RPC join; all are
// voters, and each (unless Diskless) gets one disk. Returns once every node sees the
// same leader and membership.
func NewCluster(t *testing.T, n int, k, m uint8, opts ...ClusterOption) *Cluster {
	t.Helper()
	o := clusterOpts{chunkSize: ChunkSize}
	for _, f := range opts {
		f(&o)
	}
	c := &Cluster{T: t, Clk: clock.NewFake(time.Now()), K: k, M: m, snap: o.snapshotThreshold, retryUntilStale: o.retryUntilStale, nonVoters: o.nonVoters, auto: o.auto, walCap: o.walCap, diskless: o.diskless}
	for i := range n {
		c.Nodes = append(c.Nodes, c.newNode(i))
	}
	first := c.Nodes[0]
	first.start(c, true, meta.SetVolume{ChunkSize: o.chunkSize, K: k, M: m, Now: 1})
	for _, nd := range c.Nodes[1:] {
		nd.start(c, false, meta.SetVolume{})
		c.join(nd)
	}
	c.Settle()
	for i := range n {
		if !c.diskless[i] {
			c.AddDisk(i)
		}
	}
	return c
}

func (c *Cluster) newNode(i int) *ClusterNode {
	c.T.Helper()
	dir := filepath.Join(c.T.TempDir(), fmt.Sprintf("n%d", i))
	id, err := cluster.LoadOrCreateNodeID(dir)
	if err != nil {
		c.T.Fatal(err)
	}
	// the listener stays open until the node first serves: a port reserved by listen and
	// close can be handed to another test process in between (packages test in parallel)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.T.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	nd := &ClusterNode{I: i, ID: id, Dir: dir, port: port, first: l, idx: meta.NodeIdx(i + 1)} // idx is final once the node has joined
	nd.raddr, nd.trans = raft.NewInmemTransportWithTimeout(raft.ServerAddress(id), 10*time.Second)
	inmemMu.Lock()
	inmemAddrs[id] = nd.raddr
	inmemMu.Unlock()
	return nd
}

func (nd *ClusterNode) record(c *Cluster) meta.Node {
	return meta.Node{
		ID: nd.ID, Name: fmt.Sprintf("n%d", nd.I), Kind: meta.KindDevice, TSStableID: harnessStableID, Addr: "127.0.0.1",
		RaftPort: 1, RPCPort: uint16(nd.port), Voter: c.Voter(nd.I), // no budget: a device stores nothing (docs/14 §9)
	}
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
	opts := cluster.Options{DataDir: nd.Dir, NodeID: nd.ID, ClusterID: c.CID, Transport: nd.trans, Timing: RaftTiming(), Stores: nd.stores, SnapshotThreshold: c.snap, RaftAddrFor: inmemAddrFor}
	if os.Getenv("TAILRAID_HARNESS_DEBUG") != "" {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("node", fmt.Sprintf("n%d", nd.I))
	}
	var err error
	switch {
	case bootstrap:
		nd.Node, err = cluster.Bootstrap(context.Background(), opts, nd.record(c), vol, c.auto, "harness-token")
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
	nd.Cache, err = chunk.OpenCache(sysfs.OS{}, filepath.Join(nd.Dir, "cache"), 1<<30, c.Clk)
	if err != nil {
		c.T.Fatal(err)
	}
	cs := ChunkSize
	if v := nd.Node.FSM.Volume(); v.ChunkSize != 0 {
		cs = v.ChunkSize
	}
	var ovOpts []vfs.Option
	if c.walCap > 0 {
		ovOpts = append(ovOpts, vfs.WithWALCap(c.walCap))
	}
	nd.Ov, err = vfs.OpenOverlay(filepath.Join(nd.Dir, "staging"), cs, c.Clk, ovOpts...)
	if err != nil {
		c.T.Fatal(err)
	}
	nd.Net = NewFaultNet(http.DefaultTransport)
	nd.Client = rpc.NewClient(c.CID, nd.Node.RPCAddrOf)
	nd.Client.SetTransport(nd.Net)
	nd.Client.SetDisks(nd.Node.DiskOf, nd.disks.local)
	nd.Client.SetS3(cluster.NewS3Buckets(nd.Node, nd.s3.get, nil).Bucket)
	nd.Node.SetForwarder(nd.Client)
	nd.Prober = cluster.NewProber(nd.Node, nd.Client, c.Clk)
	nd.Prober.SelfHealth = func() rpc.Health {
		return rpc.Health{Applied: nd.Node.AppliedIndex(), Disks: nd.disks.health()}
	}
	nd.Events = meta.NewEventRing(c.Clk, nd.Idx())
	nd.Reader = vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nd.Cache, nil)
	nd.Reader.Events = nd.Events
	cfg := vfs.DefaultConfig()
	cfg.PutDeadline = 2 * time.Second
	nd.Flush = vfs.NewFlusher(nd.Ov, nd.Node.FSM, nd.Node, nd.Prober, nd.Client, nd.Reader, c.Clk, cfg, nil)
	// a Commit carries shard sums only once every member reads codec 5 (I-201). The closure
	// keeps the node it was built with: a stats writer still holding these handles after a
	// restart calls it when nd.Node is another node, or nil while the node is down.
	self := nd.Node
	// the leader refuses a command newer than a live member reads (I-156), as node.go wires it
	nd.Node.MinCodec = nd.Prober.MinCodec
	nd.Flush.MinCodec = func() uint8 {
		if self.MinCodec == nil {
			return meta.CodecVersion
		}
		return self.MinCodec()
	}
	nd.Client.OnUnreachable = nd.Prober.Suspect // as node.go wires them (docs/05 §6, I-170)
	nd.Flush.Suspect = nd.Prober.Suspect
	nd.Flush.Events = nd.Events
	nd.FS = vfs.New(nd.Node.FSM, nd.Node, nd.Ov, nd.Reader, c.Clk, 501, 20)
	nd.FS.SetFlusher(nd.Flush)
	if c.retryUntilStale {
		// the Stale rule of cluster.Node.Health, measured on the harness clock alone: Raft
		// stamps LastContact on the wall clock, and comparing it with a fake clock that
		// tests advance by seconds at a time makes any follower stale once the clock has
		// run 30 s ahead. A contact is noted at the fake time it is first seen.
		var (
			hmu           sync.Mutex
			seenContact   time.Time // Raft's last-contact stamp, wall clock
			contactAt     time.Time // when it was seen, harness clock
			noLeaderSince time.Time
		)
		raftNode := nd.Node
		nd.FS.SetHealth(func() bool {
			now := c.Clk.Now()
			hmu.Lock()
			defer hmu.Unlock()
			if contactAt.IsZero() {
				contactAt = now
			}
			if raftNode.IsLeader() {
				contactAt, noLeaderSince = now, time.Time{}
				return false
			}
			if _, id := raftNode.Raft.LeaderWithID(); id == "" {
				if noLeaderSince.IsZero() {
					noLeaderSince = now
				}
				return now.Sub(noLeaderSince) >= cluster.StaleThreshold
			}
			noLeaderSince = time.Time{}
			if lc := raftNode.Raft.LastContact(); lc.After(seenContact) {
				seenContact, contactAt = lc, now
			}
			return now.Sub(contactAt) >= cluster.StaleThreshold
		})
	}
	nd.RPC, err = rpc.NewServer(rpc.Options{
		NodeID: nd.ID, ClusterID: c.CID, Version: "harness", Applier: nd.Node,
		Disk: nd.disks.lookup, DiskHealth: nd.disks.health,
		Fence: fence{nd}, Joiner: nd.Node, Auth: rpc.AllowAll{Node: meta.Node{TSStableID: harnessStableID}}, Members: nd.Node.FSM.Nodes,
		JoinToken: func() string { return nd.Node.FSM.Meta().JoinToken }, Clk: c.Clk, MaxShard: 1 << 20, Grace: 10 * time.Minute,
	})
	if err != nil {
		c.T.Fatal(err)
	}
	l := nd.first
	nd.first = nil
	if l == nil {
		// a restart binds the port the other nodes know; the previous server's socket may
		// still be closing, so wait for it rather than fail
		Eventually(c.T, func() bool {
			var err error
			l, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", nd.port))
			return err == nil
		}, fmt.Sprintf("n%d: port %d stayed busy across a restart", nd.I, nd.port))
	}
	nd.srv = &httptest.Server{Listener: l, Config: &http.Server{Handler: nd.RPC.Handler(), ReadHeaderTimeout: 5 * time.Second}}
	nd.srv.Start()
	c.setAlive(nd, true)
}

type fence struct{ nd *ClusterNode }

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
	req := rpc.JoinRequest{
		Token: "harness-token", NodeID: nd.ID, Name: fmt.Sprintf("n%d", nd.I), TSStableID: harnessStableID,
		Addr: "127.0.0.1", RaftPort: 1, RPCPort: uint16(nd.port), Voter: c.Voter(nd.I), Version: "harness",
	}
	var last error
	for range 50 {
		var resp rpc.JoinResponse
		resp, last = nd.Client.Join(context.Background(), leader.RPCAddr(), req)
		if last == nil {
			nd.idx = resp.NodeIdx
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

func splitPath(p string) []string { return meta.SplitPath(p) }

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
	for range 20 {
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
	nd.lifecycle.Lock()
	defer nd.lifecycle.Unlock()
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
	// A fetch that outlived its read (readahead, the early path) would Put into the cache
	// while it drains; a cache write still in flight would race a Restart's new cache on
	// the same directory, or the temp directory's removal after Close (I-204).
	nd.Reader.Close()
	nd.Cache.Close()
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
	nd.lifecycle.Lock()
	defer nd.lifecycle.Unlock()
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

// Recover rewrites the killed node i's Raft membership to the nodes in voters (docs/12
// §9): what `tailraid recover --voters` does on a stopped node. Restart brings it back.
func (c *Cluster) Recover(i int, voters []int) error {
	c.T.Helper()
	nd := c.Nodes[i]
	if c.isAlive(nd) {
		c.T.Fatalf("recover n%d: the node is running", i)
	}
	servers := make([]raft.Server, 0, len(voters))
	for _, v := range voters {
		servers = append(servers, raft.Server{Suffrage: raft.Voter, ID: raft.ServerID(c.Nodes[v].ID), Address: c.Nodes[v].raddr})
	}
	return cluster.Recover(cluster.Options{DataDir: nd.Dir, NodeID: nd.ID, ClusterID: c.CID, Timing: RaftTiming(), Stores: nd.stores}, servers)
}

// ResetAndRejoin does what `tailraid reset` then `tailraid join` do on a device: the
// node is stopped, its Raft state and identity dropped (a fresh node ID at the same
// address), and it joins the leader again (docs/08 §Join, I-94).
func (c *Cluster) ResetAndRejoin(i int) {
	c.T.Helper()
	nd := c.Nodes[i]
	c.Kill(i)
	nd.stores = nil
	nd.started = false
	for _, f := range []string{"node_id", "cluster_id"} {
		_ = os.Remove(filepath.Join(nd.Dir, f))
	}
	id, err := cluster.LoadOrCreateNodeID(nd.Dir)
	if err != nil {
		c.T.Fatal(err)
	}
	inmemMu.Lock()
	delete(inmemAddrs, nd.ID)
	inmemAddrs[id] = nd.raddr // the same Raft address under a new identity
	inmemMu.Unlock()
	nd.ID = id
	nd.start(c, false, meta.SetVolume{})
	for _, other := range c.Nodes {
		if c.isAlive(other) {
			other.Net.Cut(nd.RPCAddr(), false)
		}
	}
	c.join(nd)
	c.Settle()
}

// Partition splits the cluster into two sides (raft and RPC links cut across sides).
func (c *Cluster) Partition(a, b []int) {
	in := slices.Contains[[]int]
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
	for range 30 { // a flush fails across a leadership change and is retried
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
