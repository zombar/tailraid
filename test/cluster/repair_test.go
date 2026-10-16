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

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

func newRepairer(c *testharness.Cluster, nd *testharness.ClusterNode, cfg repair.Config) *repair.Repairer {
	r := repair.New(nd.Node.FSM, nd.Node, nd.Prober, nd.Client, nd.Reader, c.Clk, cfg, nil)
	r.Events = nd.Events
	return r
}

func repairCfg() repair.Config {
	cfg := repair.DefaultConfig()
	cfg.ScrubPeriod = 0
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
			if !c.StoreOf(s).Has(chunk.Key(ch.Hash, ch.K, ch.M, i)) {
				t.Fatalf("slot %d on target %d has no shard", i, s)
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
		if i != 0 && (nd.Target() == ch.Placement[0] || nd.Target() == ch.Placement[1] || nd.Target() == ch.Placement[2]) {
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
		if s == 0 || s == c.Nodes[victim].Target() {
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
	stale := chunk.Key(ch.Hash, ch.K, ch.M, slotOf(ch, c.Nodes[victim].Target()))
	c.Clk.Advance(11 * time.Minute)
	n, err := sweepNode(c, victim)
	must(t, err)
	if n != 1 || c.Nodes[victim].Store.Has(stale) {
		t.Fatalf("sweep removed %d, stale present %v", n, c.Nodes[victim].Store.Has(stale))
	}
}

// holdersBySlot returns the node indexes (into c.Nodes) of ch's holders in slot order,
// excluding the leader so they can be killed or drained without an election.
func holdersBySlot(c *testharness.Cluster, ch meta.Chunk, leader *testharness.ClusterNode) []int {
	var out []int
	for _, s := range ch.Placement {
		if s == 0 || s == leader.Target() {
			continue
		}
		out = append(out, c.HostOf(s))
	}
	return out
}

// A missing slot with no live spare, plus a holder that is dead but inside fill_after:
// nothing is placeable, so the pass must propose nothing rather than re-commit the same
// placement every minute and count it as a partial repair.
// Invariant: I-65
// Lifecycle: L-86, L-185
func TestRepair_DeadWithinFillAfter_NoRepeatProposal(t *testing.T) {
	c := testharness.NewCluster(t, 5, 2, 2)
	defer c.Close()
	leader := c.Leader()
	// two nodes down: four slots, three live nodes, so the write lands with one zero slot
	// (a 3-of-5 quorum remains)
	c.Kill((leader.I + 1) % 5)
	c.Kill((leader.I + 2) % 5)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	data := testharness.Pattern(int(testharness.ChunkSize), 31)
	c.WriteFile(leader.I, "/f", data)
	c.Settle()
	ch := c.ChunksOf(leader.I, "/f")[0]
	zero := 0
	for _, s := range ch.Placement {
		if s == 0 {
			zero++
		}
	}
	if zero != 1 {
		t.Fatalf("expected one zero slot after a degraded write, got placement %v", ch.Placement)
	}
	// a holder becomes unreachable for the leader's prober (RPC only, so Raft keeps its
	// quorum) and stays inside fill_after: its slot is kept, not re-placed
	victim := holdersBySlot(c, ch, leader)[0]
	leader.Net.Cut(c.Nodes[victim].RPCAddr(), true)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	leader.Prober.Tick(ctx)
	if leader.Prober.IsLive(c.Nodes[victim].Idx()) {
		t.Fatal("victim still live after the cut")
	}
	r := newRepairer(c, leader, repairCfg())
	for pass := 1; pass <= 2; pass++ {
		st, err := r.Scan(ctx)
		must(t, err)
		if st.Partial != 0 || st.Repaired != 0 || st.Unrepairable != 0 {
			t.Fatalf("pass %d: nothing was placeable yet the scan reports %+v", pass, st)
		}
	}
	for _, k := range leader.Events.Kinds(ch.Hash) {
		if k == meta.EvPartial || k == meta.EvRepaired {
			t.Fatalf("a no-op pass recorded %v: %v", k, leader.Events.Of(ch.Hash))
		}
	}
	if after := c.ChunksOf(leader.I, "/f")[0]; !equalPlacement(after.Placement, ch.Placement) {
		t.Fatalf("placement changed %v -> %v", ch.Placement, after.Placement)
	}
}

func equalPlacement(a, b []meta.NodeIdx) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// One holder is dead past fill_after, another is draining, and exactly one spare
// appears: the spare replaces the dead node's slot and the draining node keeps its slot
// (and its shard) until a replacement for it exists. Zeroing it would have cut the
// readable copies from four to three while the node was live and holding the shard.
// Invariant: I-65
// Lifecycle: L-93, L-184
func TestRepair_DrainNoReplacement_KeepsShard(t *testing.T) {
	c := testharness.NewCluster(t, 5, 2, 2)
	defer c.Close()
	leader := c.Leader()
	spare := (leader.I + 1) % 5
	c.Kill(spare)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	data := testharness.Pattern(int(testharness.ChunkSize), 32)
	c.WriteFile(leader.I, "/f", data)
	c.Settle()
	ch := c.ChunksOf(leader.I, "/f")[0]
	holders := holdersBySlot(c, ch, leader)
	if len(holders) < 2 {
		t.Fatalf("placement %v leaves fewer than two non-leader holders", ch.Placement)
	}
	dead, draining := holders[0], holders[1] // the earlier slot dies so the spare is spent on it first
	c.Kill(dead)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe() // observed dead...
	c.Clk.Advance(cluster.FillAfter)
	c.Probe() // ...and past the hold-down
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[draining].Idx(), State: meta.Draining}))
	c.Restart(spare)
	c.Settle()
	if leader.Prober.IsLive(c.Nodes[dead].Idx()) || leader.Prober.DeadFor(c.Nodes[dead].Idx()) < cluster.FillAfter {
		t.Fatal("dead node not past fill_after")
	}
	st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
	must(t, err)
	if st.Unrepairable != 0 || st.Errors != 0 {
		t.Fatalf("scan %+v", st)
	}
	after := c.ChunksOf(leader.I, "/f")[0]
	drainKey := chunk.Key(after.Hash, after.K, after.M, slotOf(ch, c.Nodes[draining].Target()))
	filled := 0
	for _, s := range after.Placement {
		if s != 0 {
			filled++
		}
		if s == c.Nodes[dead].Target() {
			t.Fatalf("dead node still placed: %v", after.Placement)
		}
	}
	if filled != 4 {
		t.Fatalf("placement %v lost a readable slot (was %v)", after.Placement, ch.Placement)
	}
	if after.Placement[slotOf(ch, c.Nodes[draining].Target())] != c.Nodes[draining].Target() {
		t.Fatalf("draining node's slot was reassigned without a replacement: %v", after.Placement)
	}
	if !c.Nodes[draining].Store.Has(drainKey) {
		t.Fatal("draining node's shard was deleted")
	}
	if got, err := c.ReadFileCold(spare, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after repair: %v", err)
	}
}

// A chunk whose only live copies include a draining node is still readable, so the
// scanner must not count it unrepairable (and must not propose anything if no spare
// can take the slots).
// Invariant: I-65
// Lifecycle: L-93
func TestRepair_DrainingHolderIsReadable(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := c.Leader()
	data := testharness.Pattern(int(testharness.ChunkSize), 33)
	c.WriteFile(leader.I, "/f", data)
	c.Settle()
	ch := c.ChunksOf(leader.I, "/f")[0]
	holders := holdersBySlot(c, ch, leader)
	dead, draining := holders[0], holders[1]
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[draining].Idx(), State: meta.Draining}))
	c.Kill(dead)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	c.Clk.Advance(cluster.FillAfter)
	c.Probe()
	st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
	must(t, err)
	if st.Unrepairable != 0 {
		t.Fatalf("readable chunk (leader + live draining holder) reported unrepairable: %+v", st)
	}
	if st.Partial != 0 || st.Repaired != 0 {
		t.Fatalf("no spare exists yet the scan reports %+v", st)
	}
	after := c.ChunksOf(leader.I, "/f")[0]
	if after.Placement[slotOf(ch, c.Nodes[draining].Target())] != c.Nodes[draining].Target() {
		t.Fatalf("draining node's slot dropped: %v", after.Placement)
	}
	if got, err := c.ReadFileCold(leader.I, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %v", err)
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

// Invariant: I-32, I-42
// Lifecycle: L-93, L-94, L-161
func TestRepair_DrainMovesAllSlotsBeforeRemove(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	for i := range 4 {
		c.WriteFile(0, "/f"+string(rune('a'+i)), testharness.Pattern(int(testharness.ChunkSize)+1, byte(i)))
	}
	c.Settle()
	leader := c.Leader()
	drained := 3
	if leader.I == 3 {
		drained = 2
	}
	for i := 0; leader.Node.SlotsHeldBy(c.Nodes[drained].Target()) == 0; i++ { // never skip: more files until the node holds a slot
		if i == 8 {
			t.Fatal("placement avoided the node to be drained across twelve files")
		}
		c.WriteFile(0, fmt.Sprintf("/g%d", i), testharness.Pattern(int(testharness.ChunkSize)+1, byte(10+i)))
		c.Settle()
	}
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[drained].Idx(), State: meta.Draining}))
	c.Settle()
	st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
	must(t, err)
	if leader.Node.SlotsHeldBy(c.Nodes[drained].Target()) != 0 {
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
	// now remove succeeds without --force (the disk, then its host), and every file still reads
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[drained].Target()}))
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[drained].Idx()}))
	c.Settle()
	for i := range 4 {
		if got, err := c.ReadFileCold(0, "/f"+string(rune('a'+i))); err != nil || !bytes.Equal(got, testharness.Pattern(int(testharness.ChunkSize)+1, byte(i))) {
			t.Fatalf("file %d after remove: %v", i, err)
		}
	}
}

// A full-size chunk written while the policy was k=1 is not a "small chunk": after
// set-redundancy it must migrate to the new (k, m), while a genuinely small chunk keeps
// k=1 and follows only M. Before the shared derivation the scanner treated every k=1
// chunk of at most 64 KiB as small by design and never migrated it.
// Invariant: I-15, I-56
// Lifecycle: L-100
func TestRepair_SmallChunk_SetRedundancy_Migrates(t *testing.T) {
	c := testharness.NewCluster(t, 3, 1, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize)+100, 35) // one full chunk + a 100-byte tail
	c.WriteFile(0, "/f", data)
	c.Settle()
	chunks := c.ChunksOf(0, "/f")
	if len(chunks) != 2 || chunks[0].K != 1 || chunks[1].K != 1 {
		t.Fatalf("chunks %+v", chunks)
	}
	leader := c.Leader()
	res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: 2, M: 1, Now: c.Clk.Now().UnixNano()}})
	must(t, err)
	must(t, res.Err)
	c.Settle()
	st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
	must(t, err)
	if st.Migrated != 1 || st.PolicyStale != 0 {
		t.Fatalf("scan %+v, want the full chunk migrated and nothing stale", st)
	}
	after := c.ChunksOf(0, "/f")
	if after[0].K != 2 || after[0].M != 1 {
		t.Fatalf("full chunk still on %d+%d", after[0].K, after[0].M)
	}
	if after[1].K != 1 || after[1].M != 1 {
		t.Fatalf("small chunk moved to %d+%d", after[1].K, after[1].M)
	}
	if got, err := c.ReadFileCold(1, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after migration: %v", err)
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
			if s == 0 || !c.StoreOf(s).Has(chunk.Key(ch.Hash, 3, 2, i)) {
				t.Fatalf("new slot %d missing on target %d", i, s)
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
			for slot := range 3 {
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

// Lifecycle: L-165, L-105, L-110, L-130, L-111, L-166
func TestRepair_ReportedBadSlot_AndScrub_RepairCorruption(t *testing.T) {
	// the repair's output is verified against the hash before SetPlacement (L-165): after
	// each repair every node, cold, reads back the original bytes
	readAllNodes := func(c *testharness.Cluster, want []byte) {
		t.Helper()
		for i := range c.Nodes {
			if got, err := c.ReadFileCold(i, "/f"); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("n%d after repair: %v", i, err)
			}
		}
	}
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 9)
	c.WriteFile(0, "/f", data)
	c.Settle()
	ch := c.ChunksOf(0, "/f")[0]
	leader := c.Leader()
	r := newRepairer(c, leader, repairCfg())
	// corrupt one slot on disk; a cold read through another node reports it
	victim := c.HostOf(ch.Placement[2])
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
	if s == 0 || !c.StoreOf(s).Has(chunk.Key(after.Hash, after.K, after.M, 2)) {
		t.Fatalf("slot 2 not restored: %v", after.Placement)
	}
	readAllNodes(c, data)
	// scrub finds silent corruption without a report
	corruptShardFile(t, c.StoreOf(s).Path(chunk.Key(after.Hash, after.K, after.M, 2)))
	cfg := repairCfg()
	cfg.ScrubPeriod = time.Nanosecond // a fresh repairer's first pass then scrubs every chunk (I-215)
	st, err = newRepairer(c, c.Leader(), cfg).Scan(ctx)
	must(t, err)
	if st.Scrubbed != 1 || st.ScrubBad != 1 || st.Repaired != 1 {
		t.Fatalf("scrub %+v", st)
	}
	readAllNodes(c, data)
}

// A node that is not Healthy, or has not applied the log as far as the leader had, never
// sweeps: its chunk table could call a committed shard an orphan (forbidden L-177).
// Invariant: I-13
// Lifecycle: L-171, L-177
func TestSweep_SkippedWhenNotHealthy(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/f", testharness.Pattern(int(testharness.ChunkSize), 61))
	c.Settle()
	nd := c.Nodes[1]
	stray := testharness.Pattern(64, 62)
	var h meta.Hash
	copy(h[:], stray)
	key := chunk.Key(h, 2, 1, 0) // no chunk entry: an orphan once old enough
	if _, err := nd.Store.Put(key, stray); err != nil {
		t.Fatal(err)
	}
	ageShardFile(t, nd.Store.Path(key), 11*time.Minute)
	want := c.Leader().Node.Raft.AppliedIndex()
	c.Settle()
	sweep := func(healthy bool, applied uint64) int {
		n, err := repair.SweepLocal(ctx, nd.Store, nd.Node.FSM, nd.Target(), 10*time.Minute, c.Clk,
			func() bool { return healthy }, func() uint64 { return applied }, func() (uint64, bool) { return want, true })
		must(t, err)
		return n
	}
	if n := sweep(false, nd.Node.Raft.AppliedIndex()); n != 0 || !nd.Store.Has(key) {
		t.Fatalf("not healthy: swept %d", n)
	}
	if n := sweep(true, want-1); n != 0 || !nd.Store.Has(key) {
		t.Fatalf("behind the leader: swept %d", n)
	}
	if n := sweep(true, nd.Node.Raft.AppliedIndex()); n != 1 || nd.Store.Has(key) {
		t.Fatalf("healthy and caught up: swept %d", n)
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
		keys[c.HostOf(s)] = chunk.Key(ch.Hash, ch.K, ch.M, i)
	}
	must(t, c.Nodes[0].FS.Remove("/f"))
	c.Settle()
	purgeTrash(t, c) // a delete reclaims nothing until the entry is purged (docs/06 §1)
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

// Grace is measured on the FSM clock, the clock that stamped TombstonedAt: a leader whose
// own clock lags the proposers (here by half an hour) must still collect a tombstone once
// the FSM clock has moved grace past it, and must still respect grace before that.
// Invariant: I-14
// Lifecycle: L-76, L-77
func TestGC_LeaderClockBehind_DoesNotStallGC(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 34)
	c.WriteFile(0, "/f", data)
	c.Settle()
	ch := c.ChunksOf(0, "/f")[0]
	must(t, c.Nodes[0].FS.Remove("/f"))
	c.Settle()
	purgeTrash(t, c) // a delete reclaims nothing until the entry is purged (docs/06 §1)
	leader := c.Leader()
	behind := clock.NewFake(c.Clk.Now().Add(-30 * time.Minute))
	gc := repair.NewGC(leader.Node.FSM, leader.Node, leader.Client, leader.Prober, behind, 10*time.Minute, nil)
	res, err := gc.RunOnce(ctx)
	must(t, err)
	if res.Victims != 0 {
		t.Fatalf("collected inside grace: %+v", res)
	}
	// the FSM clock advances through the proposers' commands; the leader's clock does not
	c.Clk.Advance(11 * time.Minute)
	c.WriteFile(0, "/g", data[:4096])
	c.Settle()
	if leader.Node.FSM.LastNow() < c.Clk.Now().UnixNano() {
		t.Fatalf("FSM clock %d behind the proposers' %d", leader.Node.FSM.LastNow(), c.Clk.Now().UnixNano())
	}
	res, err = gc.RunOnce(ctx)
	must(t, err)
	if res.Victims != 1 {
		t.Fatalf("GC stalled behind the leader's clock: %+v", res)
	}
	if _, ok := leader.Node.FSM.GetChunk(ch.Hash); ok {
		t.Fatal("entry survived GC")
	}
}

// Invariant: I-13, I-14
// Lifecycle: L-54, L-85, L-80
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
		if s == 0 || !c.StoreOf(s).Has(chunk.Key(ch.Hash, ch.K, ch.M, i)) {
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
	return repair.SweepLocal(ctx, nd.Store, nd.Node.FSM, nd.Target(), 10*time.Minute, c.Clk,
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

// failPuts wraps a node's shard client so repair PUTs can be made to fail without taking
// the target node down (its health probes still answer).
type failPuts struct {
	repair.Shards
	mu    sync.Mutex
	after int          // successful PUTs allowed before every PUT fails; -1 = unlimited
	to    meta.NodeIdx // when non-zero, only PUTs to this node fail
	puts  int
}

func (f *failPuts) Put(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if (f.to == 0 || node == f.to) && f.after >= 0 && f.puts >= f.after {
		return errors.New("injected put failure")
	}
	f.puts++
	return f.Shards.Put(ctx, node, key, body)
}

func repairerWith(c *testharness.Cluster, nd *testharness.ClusterNode, shards repair.Shards) *repair.Repairer {
	return repairerWithCfg(c, nd, shards, repairCfg())
}

func repairerWithCfg(c *testharness.Cluster, nd *testharness.ClusterNode, shards repair.Shards, cfg repair.Config) *repair.Repairer {
	r := repair.New(nd.Node.FSM, nd.Node, nd.Prober, shards, nd.Reader, c.Clk, cfg, nil)
	r.Events = nd.Events
	return r
}

// A repair whose PUT fails leaves the committed placement alone and is retried on the next
// pass; nothing is proposed for a slot that did not land, and a failed attempt does not
// make the chunk's class stuck (I-380).
// Invariant: I-380
// Lifecycle: L-90
func TestRepair_PutFails_Retries(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	leader := c.Leader()
	spare := (leader.I + 1) % 4
	c.Kill(spare)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	data := testharness.Pattern(int(testharness.ChunkSize), 66)
	c.WriteFile(leader.I, "/f", data)
	c.Settle()
	ch := c.ChunksOf(leader.I, "/f")[0]
	victim := holdersBySlot(c, ch, leader)[0]
	c.Kill(victim)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	c.Clk.Advance(cluster.FillAfter)
	c.Probe()
	c.Restart(spare)
	c.Settle()
	flaky := &failPuts{Shards: leader.Client, after: 0, to: c.Nodes[spare].Target()}
	r := repairerWith(c, leader, flaky)
	st, err := r.Scan(ctx)
	must(t, err)
	if st.Repaired != 0 || st.Partial != 0 {
		t.Fatalf("a failed PUT was counted as progress: %+v", st)
	}
	if after := c.ChunksOf(leader.I, "/f")[0]; !equalPlacement(after.Placement, ch.Placement) {
		t.Fatalf("placement changed although the PUT failed: %v -> %v", ch.Placement, after.Placement)
	}
	flaky.after = -1 // the spare accepts PUTs again
	st, err = r.Scan(ctx)
	must(t, err)
	if st.Repaired != 1 {
		t.Fatalf("retry on the next pass: %+v", st)
	}
	if got, err := c.ReadFileCold(spare, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after the retried repair: %v", err)
	}
}

// A leader that dies in the middle of a policy migration leaves every chunk either on the
// old placement or fully on the new one; the next leader's pass finishes the migration and
// every file reads back from every node.
// Invariant: I-15, I-32
// Lifecycle: L-102
func TestRepair_MigrationCrash_Resumes(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	var data [][]byte
	for i := range 3 {
		data = append(data, testharness.Pattern(int(testharness.ChunkSize), byte(70+i)))
		c.WriteFile(0, "/f"+string(rune('a'+i)), data[i])
	}
	c.Settle()
	leader := c.Leader()
	res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: 3, M: 1, Now: c.Clk.Now().UnixNano()}})
	must(t, err)
	must(t, res.Err)
	c.Settle()
	// the first chunk's four PUTs land, then the leader "dies": every later PUT fails.
	// The crash point counts PUTs in order, so this pass runs one chunk at a time.
	dying := &failPuts{Shards: leader.Client, after: 4}
	serial := repairCfg()
	serial.Workers = 1
	st, err := repairerWithCfg(c, leader, dying, serial).Scan(ctx)
	must(t, err)
	if st.Migrated != 1 {
		t.Fatalf("expected exactly one chunk migrated before the crash: %+v", st)
	}
	c.Kill(leader.I)
	c.Settle()
	next := c.Leader()
	// three nodes are left for four slots: the two remaining chunks migrate with one zero
	// slot each (migrated and partial), nothing stays on the old policy
	st, err = newRepairer(c, next, repairCfg()).Scan(ctx)
	must(t, err)
	if st.Migrated != 2 || st.Partial != 2 || st.PolicyStale != 0 || st.Deferred != 0 || st.Unrepairable != 0 {
		t.Fatalf("the next leader's pass: %+v", st)
	}
	for i := range 3 {
		for _, ch := range c.ChunksOf(next.I, "/f"+string(rune('a'+i))) {
			if ch.K != 3 || ch.M != 1 || len(ch.Placement) != 4 {
				t.Fatalf("chunk of file %d on %d+%d with %d slots", i, ch.K, ch.M, len(ch.Placement))
			}
		}
		for j := range c.Nodes {
			if !c.Alive(j) {
				continue
			}
			if got, err := c.ReadFileCold(j, "/f"+string(rune('a'+i))); err != nil || !bytes.Equal(got, data[i]) {
				t.Fatalf("n%d file %d after the resumed migration: %v", j, i, err)
			}
		}
	}
}
