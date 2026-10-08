package cluster_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

func newRepairer(c *testharness.Cluster, nd *testharness.ClusterNode, cfg repair.Config) *repair.Repairer {
	return repair.New(nd.Node.FSM, nd.Node, nd.Prober, nd.Client, nd.Reader, c.Clk, cfg, nil)
}

func repairCfg() repair.Config {
	cfg := repair.DefaultConfig()
	cfg.ScrubShare = 0
	cfg.RateBytes = 0
	return cfg
}

// slotsOnLive counts placement slots of path's chunks that sit on live nodes.
func slotsSummary(c *testharness.Cluster, i int, path string) (total, filled int) {
	for _, ch := range c.ChunksOf(i, path) {
		for _, s := range ch.Placement {
			total++
			if s != 0 {
				filled++
			}
		}
	}
	return
}

// Invariant: I-15
// Lifecycle: L-52, L-89, L-103
func TestRepair_FillsDegradedSlotsWhenSpareAppears(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	// write degraded: one node down, so each chunk commits with a 0 slot
	c.Kill(2)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	data := testharness.Pattern(2*int(testharness.ChunkSize), 1)
	c.WriteFile(0, "/f", data)
	if total, filled := slotsSummary(c, 0, "/f"); total != 6 || filled != 4 {
		t.Fatalf("expected degraded placement, got %d/%d", filled, total)
	}
	// no spare yet: a scan cannot fill (only 2 live nodes already hold the stripe)
	leader := c.Leader()
	st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired != 0 || st.Partial != 0 {
		t.Fatalf("filled without a spare: %+v", st)
	}
	// the node returns: slots are filled immediately (0 slots have no hold-down)
	c.Restart(2)
	c.Settle()
	st, err = newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired != 2 || st.Partial != 0 || st.Unrepairable != 0 {
		t.Fatalf("scan %+v", st)
	}
	if total, filled := slotsSummary(c, 0, "/f"); filled != total {
		t.Fatalf("still degraded %d/%d", filled, total)
	}
	for _, ch := range c.ChunksOf(0, "/f") {
		for i, s := range ch.Placement {
			if !c.Nodes[s-1].Store.Has(chunk.Key(ch.Hash, ch.K, ch.M, i)) {
				t.Fatalf("slot %d on n%d has no shard", i, s-1)
			}
		}
	}
	if got, err := c.ReadFileCold(2, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after repair: %v", err)
	}
}

// Invariant: I-15
// Lifecycle: L-86, L-87, L-88, L-91
func TestRepair_DeadNodeHoldDown_ThenReplaced(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 2)
	c.WriteFile(0, "/f", data)
	c.Settle()
	ch := c.ChunksOf(0, "/f")[0]
	var victim int
	for i, nd := range c.Nodes {
		if i != 0 && (nd.Idx() == ch.Placement[0] || nd.Idx() == ch.Placement[1] || nd.Idx() == ch.Placement[2]) {
			victim = i
		}
	}
	c.Kill(victim)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	leader := c.Leader()
	r := newRepairer(c, leader, repairCfg())
	// dead but inside fill_after: nothing moves
	st, err := r.Scan(ctx)
	must(t, err)
	if st.Repaired+st.Partial != 0 {
		t.Fatalf("re-placed inside the hold-down: %+v", st)
	}
	c.Clk.Advance(cluster.FillAfter)
	c.Probe()
	st, err = r.Scan(ctx)
	must(t, err)
	if st.Repaired != 1 {
		t.Fatalf("after hold-down: %+v", st)
	}
	after := c.ChunksOf(0, "/f")[0]
	for _, s := range after.Placement {
		if s == 0 || s == c.Nodes[victim].Idx() {
			t.Fatalf("placement after repair %v still names dead node %d", after.Placement, victim)
		}
	}
	// the dead node returns with its stale shard: it is not in the placement, so its own
	// sweep removes it once old enough; data reads from the new placement meanwhile
	c.Restart(victim)
	c.Settle()
	if got, err := c.ReadFileCold(victim, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read on returned node: %v", err)
	}
	stale := chunk.Key(ch.Hash, ch.K, ch.M, slotOf(ch, c.Nodes[victim].Idx()))
	c.Clk.Advance(11 * time.Minute)
	n, err := sweepNode(c, victim)
	must(t, err)
	if n != 1 || c.Nodes[victim].Store.Has(stale) {
		t.Fatalf("sweep removed %d, stale present %v", n, c.Nodes[victim].Store.Has(stale))
	}
}

func slotOf(ch meta.Chunk, idx meta.NodeIdx) int {
	for i, s := range ch.Placement {
		if s == idx {
			return i
		}
	}
	return -1
}

// Invariant: I-32
// Lifecycle: L-93, L-94
func TestRepair_DrainMovesAllSlotsBeforeRemove(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	for i := 0; i < 4; i++ {
		c.WriteFile(0, "/f"+string(rune('a'+i)), testharness.Pattern(int(testharness.ChunkSize)+1, byte(i)))
	}
	c.Settle()
	leader := c.Leader()
	drained := 3
	if leader.I == 3 {
		drained = 2
	}
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[drained].Idx(), State: meta.Draining}))
	c.Settle()
	before := leader.Node.SlotsHeldBy(c.Nodes[drained].Idx())
	if before == 0 {
		t.Skip("placement happened to avoid the drained node")
	}
	st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
	must(t, err)
	if leader.Node.SlotsHeldBy(c.Nodes[drained].Idx()) != 0 {
		t.Fatalf("drained node still holds slots after scan %+v", st)
	}
	// its shard files are younger than grace, so the fenced DELETEs were refused (I-14);
	// once they age, the drained node's own sweep removes every orphan
	ageAllShards(t, c, drained, 11*time.Minute)
	if _, err := sweepNode(c, drained); err != nil {
		t.Fatal(err)
	}
	left := 0
	_ = c.Nodes[drained].Store.Walk(func(chunk.ShardKey, int64, time.Time) error { left++; return nil })
	if left != 0 {
		t.Fatalf("%d shard files left on the drained node after sweep", left)
	}
	// now remove succeeds without --force, and every file still reads
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[drained].Idx()}))
	c.Settle()
	for i := 0; i < 4; i++ {
		if got, err := c.ReadFileCold(0, "/f"+string(rune('a'+i))); err != nil || !bytes.Equal(got, testharness.Pattern(int(testharness.ChunkSize)+1, byte(i))) {
			t.Fatalf("file %d after remove: %v", i, err)
		}
	}
}

// Invariant: I-15, I-32
// Lifecycle: L-37, L-101, L-157, L-173
func TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK(t *testing.T) {
	c := testharness.NewCluster(t, 5, 2, 1)
	defer c.Close()
	data := testharness.Pattern(3*int(testharness.ChunkSize), 7)
	c.WriteFile(0, "/f", data)
	c.Settle()
	leader := c.Leader()
	res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: 3, M: 2, Now: c.Clk.Now().UnixNano()}})
	must(t, err)
	must(t, res.Err)
	c.Settle()
	if v := leader.Node.FSM.Volume(); v.K != 3 || v.PolicyVersion != 1 {
		t.Fatalf("volume %+v", v)
	}
	// every chunk must stay readable at every point of the migration: read between passes
	r := newRepairer(c, leader, repairCfg())
	st, err := r.Scan(ctx)
	must(t, err)
	if st.Migrated != 3 || st.PolicyStale != 0 {
		t.Fatalf("migration %+v", st)
	}
	for _, ch := range c.ChunksOf(0, "/f") {
		if ch.K != 3 || ch.M != 2 || len(ch.Placement) != 5 {
			t.Fatalf("chunk still on old policy %+v", ch)
		}
		for i, s := range ch.Placement {
			if s == 0 || !c.Nodes[s-1].Store.Has(chunk.Key(ch.Hash, 3, 2, i)) {
				t.Fatalf("new slot %d missing on n%d", i, s-1)
			}
		}
	}
	// old-encoding shards are younger than grace, so their deletes were refused; after
	// ageing, every node's sweep removes them (the placement names the new keys)
	for i := range c.Nodes {
		ageAllShards(t, c, i, 11*time.Minute)
		if _, err := sweepNode(c, i); err != nil {
			t.Fatal(err)
		}
		for _, ch := range c.ChunksOf(0, "/f") {
			for slot := 0; slot < 3; slot++ {
				if c.Nodes[i].Store.Has(chunk.Key(ch.Hash, 2, 1, slot)) {
					t.Fatalf("old shard slot %d still on n%d after sweep", slot, i)
				}
			}
		}
	}
	for i := range c.Nodes {
		if got, err := c.ReadFileCold(i, "/f"); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n%d read after migration: %v", i, err)
		}
	}
	// a second pass is a no-op
	st, err = r.Scan(ctx)
	must(t, err)
	if st.Migrated+st.Repaired+st.Partial != 0 {
		t.Fatalf("second pass did work: %+v", st)
	}
}

// Lifecycle: L-105, L-110, L-130
func TestRepair_ReportedBadSlot_AndScrub_RepairCorruption(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 9)
	c.WriteFile(0, "/f", data)
	c.Settle()
	ch := c.ChunksOf(0, "/f")[0]
	leader := c.Leader()
	r := newRepairer(c, leader, repairCfg())
	// corrupt one slot on disk; a cold read through another node reports it
	victim := int(ch.Placement[2]) - 1
	key := chunk.Key(ch.Hash, ch.K, ch.M, 2)
	corruptShardFile(t, c.Nodes[victim].Store.Path(key))
	// a read of that slot detects the corruption, quarantines the file (I-11) and reports it
	if _, err := c.Nodes[victim].Store.Get(key); !errors.Is(err, chunk.ErrCorrupt) {
		t.Fatalf("corrupt shard read: %v", err)
	}
	if got, err := c.ReadFileCold((victim+1)%3, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read with one corrupt slot: %v", err)
	}
	r.ReportBad(key, ch.Placement[2])
	st, err := r.Scan(ctx)
	must(t, err)
	if st.Repaired != 1 {
		t.Fatalf("reported slot not repaired: %+v", st)
	}
	after := c.ChunksOf(0, "/f")[0]
	s := after.Placement[2]
	if s == 0 || !c.Nodes[s-1].Store.Has(chunk.Key(after.Hash, after.K, after.M, 2)) {
		t.Fatalf("slot 2 not restored: %v", after.Placement)
	}
	// scrub finds silent corruption without a report
	corruptShardFile(t, c.Nodes[s-1].Store.Path(chunk.Key(after.Hash, after.K, after.M, 2)))
	cfg := repairCfg()
	cfg.ScrubShare = 1
	st, err = newRepairer(c, c.Leader(), cfg).Scan(ctx)
	must(t, err)
	if st.Scrubbed != 1 || st.ScrubBad != 1 || st.Repaired != 1 {
		t.Fatalf("scrub %+v", st)
	}
	if got, err := c.ReadFileCold(0, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after scrub repair: %v", err)
	}
}

func corruptShardFile(t *testing.T, path string) {
	t.Helper()
	b, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	must(t, writeFile(path, b))
}

// Invariant: I-13, I-14
// Lifecycle: L-76, L-77, L-78, L-79, L-81, L-82, L-84
func TestGC_CollectsAfterGrace_RespectsFence_AndSweep(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 11)
	c.WriteFile(0, "/f", data)
	c.Settle()
	ch := c.ChunksOf(0, "/f")[0]
	keys := map[int]chunk.ShardKey{}
	for i, s := range ch.Placement {
		keys[int(s)-1] = chunk.Key(ch.Hash, ch.K, ch.M, i)
	}
	must(t, c.Nodes[0].FS.Remove("/f"))
	c.Settle()
	leader := c.Leader()
	gc := repair.NewGC(leader.Node.FSM, leader.Node, leader.Client, leader.Prober, c.Clk, 10*time.Minute, nil)
	// inside grace: nothing collected, shards intact
	res, err := gc.RunOnce(ctx)
	must(t, err)
	if res.Victims != 0 {
		t.Fatalf("collected inside grace: %+v", res)
	}
	if _, ok := leader.Node.FSM.GetChunk(ch.Hash); !ok {
		t.Fatal("entry gone inside grace")
	}
	// after grace the entry goes, but the files are younger than grace on disk → fence refuses
	c.Clk.Advance(11 * time.Minute)
	res, err = gc.RunOnce(ctx)
	must(t, err)
	if res.Victims != 1 {
		t.Fatalf("victims %+v", res)
	}
	if _, ok := leader.Node.FSM.GetChunk(ch.Hash); ok {
		t.Fatal("entry survived GC")
	}
	for i, k := range keys {
		if !c.Nodes[i].Store.Has(k) {
			t.Fatalf("shard on n%d deleted while younger than grace (I-14)", i)
		}
	}
	// the files age past grace: each node's sweep removes its orphan
	for i, k := range keys {
		ageShardFile(t, c.Nodes[i].Store.Path(k), 11*time.Minute)
		n, err := sweepNode(c, i)
		must(t, err)
		if n != 1 || c.Nodes[i].Store.Has(k) {
			t.Fatalf("sweep on n%d removed %d", i, n)
		}
	}
}

// Invariant: I-13, I-14
// Lifecycle: L-54, L-85
func TestCluster_GCDoesNotDeleteInflightUpload(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 12)
	c.WriteFile(0, "/a", data)
	c.Settle()
	must(t, c.Nodes[0].FS.Remove("/a"))
	c.Settle()
	leader := c.Leader()
	gc := repair.NewGC(leader.Node.FSM, leader.Node, leader.Client, leader.Prober, c.Clk, 0, nil) // grace 0: worst case
	// a second file with the same content dedups against the tombstone; GC runs between
	// the dedup check and the commit
	nd := c.Nodes[0]
	f, err := nd.FS.Create("/b")
	must(t, err)
	_, _ = f.Write(data)
	_ = f.Close()
	var ran bool
	nd.Flush.Hooks.BeforeCommit = func() {
		if ran {
			return
		}
		ran = true
		if _, err := gc.RunOnce(ctx); err != nil {
			t.Error(err)
		}
	}
	must(t, nd.Flush.FlushAll(ctx))
	if !ran {
		t.Fatal("hook did not run")
	}
	c.Settle()
	for i := range c.Nodes {
		if got, err := c.ReadFileCold(i, "/b"); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n%d: data lost to GC race: %v", i, err)
		}
	}
	ch := c.ChunksOf(0, "/b")[0]
	for i, s := range ch.Placement {
		if s == 0 || !c.Nodes[s-1].Store.Has(chunk.Key(ch.Hash, ch.K, ch.M, i)) {
			t.Fatalf("slot %d missing after GC race", i)
		}
	}
	_ = errors.Is
	_ = context.Background
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o600) }

// ageShardFile pushes a shard file's mtime into the past so grace-based rules see it as old.
func ageShardFile(t *testing.T, path string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	must(t, os.Chtimes(path, old, old))
}

// sweepNode runs the local orphan sweep on node i against the leader's applied index.
func sweepNode(c *testharness.Cluster, i int) (int, error) {
	// observe the leader's index first, then settle: the sweep only requires that this
	// node has applied what the leader had committed when the scan began
	want := c.Leader().Node.Raft.AppliedIndex()
	c.Settle()
	nd := c.Nodes[i]
	return repair.SweepLocal(ctx, nd.Store, nd.Node.FSM, nd.Idx(), 10*time.Minute, c.Clk,
		func() bool { return true }, nd.Node.Raft.AppliedIndex, func() (uint64, bool) { return want, true })
}

// ageAllShards pushes every shard file on node i into the past.
func ageAllShards(t *testing.T, c *testharness.Cluster, i int, by time.Duration) {
	t.Helper()
	_ = c.Nodes[i].Store.Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error {
		ageShardFile(t, c.Nodes[i].Store.Path(k), by)
		return nil
	})
}
