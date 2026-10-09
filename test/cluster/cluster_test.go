// Package cluster_test runs multi-node scenarios on the in-process harness (docs/09
// §cluster, docs/05 failure model).
package cluster_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"

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
	data = rmw(t, c, 0, "/f", data, int(testharness.ChunkSize)/3, testharness.Pattern(200, 11))
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

// Invariant: I-12, I-40
// Lifecycle: L-74, L-75, L-64, L-65
func TestCluster_KillMNodes_AllDataReadable_KillMPlusOne_EIO(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(3*int(testharness.ChunkSize), 2)
	c.WriteFile(0, "/f", data)
	c.Settle()
	data = rmw(t, c, 0, "/f", data, int(testharness.ChunkSize)+int(testharness.ChunkSize)/2, testharness.Pattern(300, 12))
	// 2+1 over three nodes places a slot on every node: the two non-gateway holders are
	// the victims, so the read drops from k to k-1 shards without killing the gateway
	_, holders := shardHolders(c, "/f", 0)
	if len(holders) != 3 {
		t.Fatalf("holders %v, want every node", holders)
	}
	victim, second := 1, 2
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
	// kill the second holder: reads fail cleanly with EIO, nothing panics
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
// Lifecycle: L-58, L-61
func TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	const n = 300
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("f%03d", i)
			for range 150 { // an election after the kill takes a few seconds
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
// Lifecycle: L-59, L-148
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
	for i := range 200 {
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
// Lifecycle: L-187
func TestCluster_IsolatedGateway_ReturnsEROFS_ReadsStillServed(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1, testharness.RetryUntilStale())
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
	// a mutation issued before the Stale threshold waits for a leader; the clock passes
	// the threshold meanwhile and the mutation fails with exactly EROFS (L-187)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				c.Clk.Advance(time.Second)
				runtime.Gosched()
			}
		}
	}()
	err := c.Nodes[iso].FS.MkdirAll("/nope", 0o755)
	close(done)
	if !isReadOnly(err) {
		t.Fatalf("isolated mkdir: %v, want EROFS", err)
	}
	if h := c.Nodes[iso].Node.Health(c.Clk, c.Nodes[iso].Prober); h.State == cluster.Healthy {
		t.Fatalf("health %+v", h)
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
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		return errors.Is(pe.Err, cluster.ErrNoLeader) || pe.Err.Error() == "read-only file system"
	}
	return false
}

func otherNodes(n, except int) []int {
	var out []int
	for i := range n {
		if i != except {
			out = append(out, i)
		}
	}
	return out
}

// Invariant: I-48, I-42
// Lifecycle: L-93, L-95, L-96, L-156, L-161
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
	// remove is refused while it holds slots (the FSM's rule, L-95); with --force every
	// slot on the node is zeroed first (L-96)
	err := leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[2].Idx()})
	if !errors.Is(err, meta.ErrNodeNotEmpty) {
		t.Fatalf("remove with slots: %v", err)
	}
	held := leader.Node.SlotsHeldBy(c.Nodes[2].Idx())
	if held == 0 {
		t.Fatal("test needs the node to hold slots")
	}
	zeroSlots := func() int {
		n := 0
		for _, path := range []string{"/f", "/g"} {
			for _, ch := range c.ChunksOf(leader.I, path) {
				for _, s := range ch.Placement {
					if s == 0 {
						n++
					}
				}
			}
		}
		return n
	}
	before := zeroSlots() // /g was written with two candidates for three slots
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[2].Idx(), Force: true}))
	c.Settle()
	if n := leader.Node.SlotsHeldBy(c.Nodes[2].Idx()); n != 0 {
		t.Fatalf("%d slots still name the removed node", n)
	}
	if zeroed := zeroSlots() - before; zeroed != held {
		t.Fatalf("%d slots zeroed by --force, want %d", zeroed, held)
	}
	// the removed node cannot join again until it is reset
	removedID := ""
	for _, nd := range leader.Node.FSM.Nodes() {
		if nd.Idx == c.Nodes[2].Idx() {
			removedID = nd.ID
		}
	}
	_, err = leader.Node.Join(ctx, rpc.JoinRequest{NodeID: removedID, Name: "again", Addr: "1.1.1.1", RaftPort: 1, RPCPort: 2, Capacity: 1})
	if !errors.Is(err, cluster.ErrBadMemberOp) {
		t.Fatalf("rejoin of a removed node: %v", err)
	}
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
	for range 100 {
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
	for range 100 {
		if err = nd.FS.MkdirAll(path, 0o755); err == nil {
			return
		}
		<-time.After(20 * time.Millisecond)
	}
	t.Fatalf("mkdir %s on n%d: %v", path, nd.I, err)
}

// An address change is a node-table update: placements are by NodeIdx and untouched, and
// every peer resolves the node to its new RPC address at once.
// Lifecycle: L-99
func TestCluster_UpdateAddr_ChangesRPCAddr(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 6)
	c.WriteFile(0, "/f", data)
	c.Settle()
	before := c.ChunksOf(0, "/f")[0].Placement
	leader := c.Leader()
	mover := (leader.I + 1) % 3
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "update_addr", NodeIdx: c.Nodes[mover].Idx(), Addr: "100.64.9.9"}))
	c.Settle()
	for _, nd := range c.Nodes {
		if !c.Alive(nd.I) {
			continue
		}
		addr, _, ok := nd.Node.RPCAddrOf(c.Nodes[mover].Idx())
		if !ok || !strings.HasPrefix(addr, "100.64.9.9:") {
			t.Fatalf("n%d resolves the moved node to %q", nd.I, addr)
		}
	}
	after := c.ChunksOf(0, "/f")[0].Placement
	if !equalPlacement(after, before) {
		t.Fatalf("placement changed on an address change: %v -> %v", before, after)
	}
}

// A gateway that has not applied a Commit yet serves the previous whole version: never a
// mixture of old and new chunks. Here the reader is cut off from the leader only (it keeps
// its other peers for shard reads and stays a follower on the old term), so it reads the
// old version until the partition heals and it applies the Commit.
// Invariant: I-36
// Lifecycle: L-73
func TestCluster_OtherGatewaySeesPreviousVersionUntilApply(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	v1 := testharness.Pattern(2*int(testharness.ChunkSize)+10, 63)
	leader := c.Leader()
	c.WriteFile(leader.I, "/f", v1)
	c.Settle()
	reader := (leader.I + 1) % 4
	c.Partition([]int{reader}, []int{leader.I})
	v2 := append(bytes.Clone(v1[:testharness.ChunkSize/2]), testharness.Pattern(2*int(testharness.ChunkSize), 64)...)
	fh, err := leader.FS.OpenFile("/f", os.O_RDWR, 0)
	must(t, err)
	if _, err := fh.Seek(int64(testharness.ChunkSize/2), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(v2[testharness.ChunkSize/2:]); err != nil {
		t.Fatal(err)
	}
	must(t, fh.Close())
	must(t, leader.Flush.FlushAll(ctx))
	if got, err := c.ReadFileCold(leader.I, "/f"); err != nil || !bytes.Equal(got, v2) {
		t.Fatalf("leader after commit: %v", err)
	}
	got, err := c.ReadFileCold(reader, "/f")
	must(t, err)
	if !bytes.Equal(got, v1) {
		t.Fatalf("cut-off gateway served something other than the previous version (len %d, diff at %d)", len(got), testharness.FirstDiff(got, v1))
	}
	c.Heal()
	c.Settle()
	if got, err := c.ReadFileCold(reader, "/f"); err != nil || !bytes.Equal(got, v2) {
		t.Fatalf("after heal: %v", err)
	}
}

// set-capacity changes a node's weight for future placements and nothing else: no
// placement and no shard inventory moves.
// Lifecycle: L-98
func TestCluster_SetCapacityDoesNotMoveData(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	for i := range 3 {
		c.WriteFile(i, "/f"+string(rune('a'+i)), testharness.Pattern(int(testharness.ChunkSize)+i, byte(65+i)))
	}
	c.Settle()
	inventory := func() map[int][]string {
		out := map[int][]string{}
		for i, nd := range c.Nodes {
			_ = nd.Store.Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error { out[i] = append(out[i], string(k)); return nil })
			sort.Strings(out[i])
		}
		return out
	}
	placements := func() [][]meta.NodeIdx {
		var out [][]meta.NodeIdx
		for i := range 3 {
			for _, ch := range c.ChunksOf(0, "/f"+string(rune('a'+i))) {
				out = append(out, ch.Placement)
			}
		}
		return out
	}
	invBefore, plBefore := inventory(), placements()
	leader := c.Leader()
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_capacity", NodeIdx: c.Nodes[(leader.I+1)%3].Idx(), Capacity: 7}))
	c.Settle()
	st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired+st.Partial+st.Migrated != 0 {
		t.Fatalf("a capacity change caused repair work: %+v", st)
	}
	if !reflect.DeepEqual(inventory(), invBefore) || !reflect.DeepEqual(placements(), plBefore) {
		t.Fatalf("data moved on set-capacity: %v -> %v", plBefore, placements())
	}
}

// The policy `init` defaults to, 1+0, is plain replication-free storage: one shard per
// chunk, no parity. An acknowledged write survives a restart of its only holder, and a
// corrupt shard is EIO, never garbage (the chunk hash is verified on every read).
// Invariant: I-12, I-40
func TestCluster_Policy1Plus0_WriteRestartRead_CorruptIsEIO(t *testing.T) {
	c := testharness.NewCluster(t, 3, 1, 0)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize)+7, 73)
	c.WriteFile(0, "/f", data)
	c.Settle()
	data = rmw(t, c, 0, "/f", data, int(testharness.ChunkSize)/2, testharness.Pattern(99, 74)) // chunk 0, the copy corrupted below
	for i := range c.Nodes {
		if got, err := c.ReadFileCold(i, "/f"); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n%d: %v", i, err)
		}
	}
	chunks := c.ChunksOf(0, "/f")
	if len(chunks) != 2 || chunks[0].K != 1 || chunks[0].M != 0 || len(chunks[0].Placement) != 1 {
		t.Fatalf("chunks %+v", chunks)
	}
	holder := int(chunks[0].Placement[0]) - 1
	reader := (holder + 1) % 3
	c.Kill(holder)
	c.Restart(holder)
	c.Settle()
	if got, err := c.ReadFileCold(reader, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("after the holder restarted: %v", err)
	}
	// the only copy is corrupt: the read is EIO, never garbage
	corruptShardFile(t, c.Nodes[holder].Store.Path(chunk.Key(chunks[0].Hash, 1, 0, 0)))
	got, err := c.ReadFileCold(reader, "/f")
	if err == nil || !errors.Is(err, vfs.ErrIO) {
		t.Fatalf("corrupt sole copy: err=%v (got %d bytes)", err, len(got))
	}
	if bytes.Equal(got[:min(len(got), len(data))], data[:min(len(got), len(data))]) && len(got) == len(data) {
		t.Fatal("corrupt sole copy returned the original bytes")
	}
}
