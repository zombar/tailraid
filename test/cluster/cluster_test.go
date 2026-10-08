// Package cluster_test runs multi-node scenarios on the in-process harness (docs/09
// §cluster, docs/05 failure model).
package cluster_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

var ctx = context.Background()

func shardHolders(c *testharness.Cluster, path string, gateway int) (meta.Chunk, []int) {
	st, _ := c.Nodes[gateway].FS.Stat(path)
	_ = st
	in, err := c.Nodes[gateway].FS.Stat(path)
	if err != nil {
		c.T.Fatal(err)
	}
	_ = in
	// find the inode's first chunk through the FSM
	var ch meta.Chunk
	c.Nodes[gateway].Node.FSM.ForEachChunk(func(x meta.Chunk) bool { ch = x; return false })
	var holders []int
	for i, nd := range c.Nodes {
		for _, slot := range ch.Placement {
			if slot == nd.Idx() {
				holders = append(holders, i)
			}
		}
	}
	return ch, holders
}

// Invariant: I-7, I-9, I-36
// Lifecycle: L-39, L-51
func TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize)+1234, 1)
	c.WriteFile(0, "/f", data)
	c.Settle()
	for i := range c.Nodes {
		got, err := c.ReadFile(i, "/f")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n%d: %v (len %d)", i, err, len(got))
		}
	}
	ch, holders := shardHolders(c, "/f", 0)
	if len(holders) != 3 || ch.K != 2 || ch.M != 1 {
		t.Fatalf("placement %v holders %v", ch.Placement, holders)
	}
	for _, slot := range ch.Placement {
		if slot == 0 {
			t.Fatalf("degraded placement %v", ch.Placement)
		}
	}
}

// Invariant: I-12
// Lifecycle: L-74, L-75
func TestCluster_KillMNodes_AllDataReadable_KillMPlusOne_EIO(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(3*int(testharness.ChunkSize), 2)
	c.WriteFile(0, "/f", data)
	c.Settle()
	// kill one non-gateway node (m = 1): every surviving gateway still reads the file
	_, holders := shardHolders(c, "/f", 0)
	victim := holders[len(holders)-1]
	if victim == 0 {
		victim = holders[0]
	}
	c.Kill(victim)
	c.Probe()
	for i := range c.Nodes {
		if !c.Alive(i) {
			continue
		}
		got, err := c.ReadFile(i, "/f")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("after killing n%d, n%d read: %v", victim, i, err)
		}
	}
	// writes continue degraded on the surviving majority
	c.WriteFile(0, "/g", []byte("still writable"))
	// kill a second holder: reads fail cleanly with EIO, nothing panics
	var second int
	for _, h := range holders {
		if h != victim && h != 0 {
			second = h
		}
	}
	if second == 0 { // gateway itself is the only other holder; read from it must still work locally
		t.Skip("gateway holds a shard; cannot drop below k without killing the gateway")
	}
	c.Kill(second)
	c.Probe()
	_, err := c.ReadFileCold(0, "/f")
	if !errors.Is(err, vfs.ErrIO) {
		t.Fatalf("read with fewer than k shards: %v", err)
	}
}

// Lifecycle: L-57
func TestCluster_FollowerProposalsForwardToLeader(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := c.Leader()
	var follower int
	for i, nd := range c.Nodes {
		if nd != leader {
			follower = i
			break
		}
	}
	if err := c.Nodes[follower].FS.MkdirAll("/from-follower/deep", 0o755); err != nil {
		t.Fatalf("mkdir via follower: %v", err)
	}
	c.WriteFile(follower, "/from-follower/deep/f", []byte("forwarded"))
	c.Settle()
	for i := range c.Nodes {
		if got, err := c.ReadFile(i, "/from-follower/deep/f"); err != nil || string(got) != "forwarded" {
			t.Fatalf("n%d: %q %v", i, got, err)
		}
	}
}

// Invariant: I-4, I-29
// Lifecycle: L-58
func TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	const n = 300
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("f%03d", i)
			for attempt := 0; attempt < 150; attempt++ { // an election after the kill takes a few seconds
				nd := c.Nodes[i%3]
				if !c.Alive(i % 3) {
					nd = c.Leader()
				}
				_, err := nd.FS.OpenFile("/"+name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
				if err == nil || errors.Is(err, os.ErrExist) {
					results[i] = nil
					return
				}
				results[i] = err
				<-time.After(100 * time.Millisecond)
			}
		}(i)
		if i == n/2 {
			leader := c.Leader()
			c.Kill(leader.I)
		}
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("f%03d never committed: %v", i, err)
		}
	}
	c.Settle()
	ents, err := c.Leader().FS.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != n {
		t.Fatalf("%d entries, want %d", len(ents), n)
	}
}

// Invariant: I-6
// Lifecycle: L-59
func TestCluster_SplitBrain_OnlyMajorityCommits(t *testing.T) {
	c := testharness.NewCluster(t, 5, 2, 1)
	defer c.Close()
	c.Partition([]int{0, 1}, []int{2, 3, 4})
	// the minority loses its leader (or never had one); its proposals fail
	testharness.Eventually(t, func() bool {
		for _, i := range []int{0, 1} {
			if c.Nodes[i].Node.IsLeader() {
				return false
			}
		}
		return true
	}, "minority steps down")
	if err := c.Nodes[0].FS.MkdirAll("/minority", 0o755); err == nil {
		t.Fatal("minority committed a write")
	}
	// the majority elects and commits
	var majLeader *testharness.ClusterNode
	testharness.Eventually(t, func() bool {
		for _, i := range []int{2, 3, 4} {
			if c.Nodes[i].Node.IsLeader() {
				majLeader = c.Nodes[i]
				return true
			}
		}
		return false
	}, "majority leader")
	mkdirRetryOn(t, majLeader, "/majority")
	c.Heal()
	c.Settle()
	for i := range c.Nodes {
		if _, err := c.Nodes[i].FS.Stat("/majority"); err != nil {
			t.Fatalf("n%d missing majority dir: %v", i, err)
		}
		if _, err := c.Nodes[i].FS.Stat("/minority"); err == nil {
			t.Fatalf("n%d has the minority dir", i)
		}
	}
}

// Invariant: I-5
// Lifecycle: L-120
func TestCluster_RejoinAfterLongAbsence_SnapshotInstall(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1, testharness.SnapshotThreshold(40))
	defer c.Close()
	c.Kill(2)
	for i := 0; i < 200; i++ {
		mkdirRetry(t, c, fmt.Sprintf("/d%03d", i))
	}
	c.WriteFile(c.Leader().I, "/payload", testharness.Pattern(5000, 3))
	c.Restart(2)
	c.Settle()
	st := c.Nodes[2].Node.FSM.Stats()
	if st.Inodes < 202 {
		t.Fatalf("rejoined node has %d inodes", st.Inodes)
	}
	if got, err := c.ReadFile(2, "/payload"); err != nil || !bytes.Equal(got, testharness.Pattern(5000, 3)) {
		t.Fatalf("read on rejoined node: %v", err)
	}
	if len(c.Nodes[2].Node.Raft.Stats()) == 0 {
		t.Fatal("no raft stats")
	}
}

// Invariant: I-39
func TestCluster_IsolatedGateway_ReturnsEROFS_ReadsStillServed(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/before", []byte("hello"))
	c.Settle()
	leader := c.Leader()
	var iso int
	for i, nd := range c.Nodes {
		if nd != leader {
			iso = i
			break
		}
	}
	c.Partition([]int{iso}, otherNodes(3, iso))
	testharness.Eventually(t, func() bool {
		h := c.Nodes[iso].Node.Health(c.Clk, c.Nodes[iso].Prober)
		return h.State != cluster.Healthy
	}, "isolated node leaves Healthy")
	c.Clk.Advance(cluster.StaleThreshold + time.Second)
	if h := c.Nodes[iso].Node.Health(c.Clk, c.Nodes[iso].Prober); h.State == cluster.Healthy {
		t.Fatalf("health %+v", h)
	}
	err := c.Nodes[iso].FS.MkdirAll("/nope", 0o755)
	if !errors.Is(err, rpc.ErrUnavailable) && !errors.Is(err, cluster.ErrNoLeader) && !isReadOnly(err) {
		t.Fatalf("isolated mkdir: %v", err)
	}
	// local metadata reads still work; data held locally is still readable
	if _, err := c.Nodes[iso].FS.Stat("/before"); err != nil {
		t.Fatalf("stat on isolated node: %v", err)
	}
	c.Heal()
	c.Settle()
	mkdirRetryOn(t, c.Nodes[iso], "/after")
}

func isReadOnly(err error) bool {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return errors.Is(pe.Err, cluster.ErrNoLeader) || pe.Err.Error() == "read-only file system"
	}
	return false
}

func otherNodes(n, except int) []int {
	var out []int
	for i := 0; i < n; i++ {
		if i != except {
			out = append(out, i)
		}
	}
	return out
}

// Invariant: I-48
// Lifecycle: L-93, L-95, L-96
func TestCluster_JoinDrainRemove(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/f", testharness.Pattern(2*int(testharness.ChunkSize), 4))
	c.Settle()
	nodes := c.Leader().Node.FSM.Nodes()
	if len(nodes) != 3 {
		t.Fatalf("members %d", len(nodes))
	}
	// drain node 2: new writes avoid it
	leader := c.Leader()
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[2].Idx(), State: meta.Draining}))
	c.Settle()
	c.WriteFile(0, "/g", testharness.Pattern(2*int(testharness.ChunkSize), 5))
	for _, ch := range c.ChunksOf(0, "/g") {
		for _, s := range ch.Placement {
			if s == c.Nodes[2].Idx() {
				t.Fatalf("draining node received a new slot: %v", ch.Placement)
			}
		}
	}
	// remove is refused while it holds slots, allowed with force
	err := leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[2].Idx()})
	if !errors.Is(err, cluster.ErrHoldsShards) {
		t.Fatalf("remove with slots: %v", err)
	}
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[2].Idx(), Force: true}))
	c.Settle()
	cfg := leader.Node.Raft.GetConfiguration()
	must(t, cfg.Error())
	if len(cfg.Configuration().Servers) != 2 {
		t.Fatalf("raft servers %d", len(cfg.Configuration().Servers))
	}
	for _, nd := range leader.Node.FSM.Nodes() {
		if nd.Idx == c.Nodes[2].Idx() && nd.State != meta.Removed {
			t.Fatalf("node state %v", nd.State)
		}
	}
	// set_capacity and set_voter round trip
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_capacity", NodeIdx: c.Nodes[1].Idx(), Capacity: 42}))
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_voter", NodeIdx: c.Nodes[1].Idx(), Voter: false}))
	c.Settle()
	for _, nd := range leader.Node.FSM.Nodes() {
		if nd.Idx == c.Nodes[1].Idx() && (nd.Capacity != 42 || nd.Voter) {
			t.Fatalf("node %+v", nd)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mkdirRetry tolerates the EROFS a gateway returns during a leadership change.
func mkdirRetry(t *testing.T, c *testharness.Cluster, path string) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		if err = c.Leader().FS.MkdirAll(path, 0o755); err == nil {
			return
		}
		<-time.After(20 * time.Millisecond)
	}
	t.Fatalf("mkdir %s: %v", path, err)
}

func mkdirRetryOn(t *testing.T, nd *testharness.ClusterNode, path string) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		if err = nd.FS.MkdirAll(path, 0o755); err == nil {
			return
		}
		<-time.After(20 * time.Millisecond)
	}
	t.Fatalf("mkdir %s on n%d: %v", path, nd.I, err)
}
