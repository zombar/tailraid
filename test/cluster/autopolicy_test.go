package cluster_test

import (
	"context"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// scanLeader runs one repair pass on the leader, where the automatic policy is evaluated.
func scanLeader(t *testing.T, c *testharness.Cluster) {
	t.Helper()
	l := c.Leader()
	rp := repair.New(l.Node.FSM, l.Node, l.Prober, l.Client, l.Reader, c.Clk, repair.DefaultConfig(), nil)
	if _, err := rp.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	c.Settle()
}

func policy(t *testing.T, c *testharness.Cluster) (k, m uint8, version uint32) {
	t.Helper()
	v := c.Leader().Node.FSM.Volume()
	return v.K, v.M, v.PolicyVersion
}

func expect(t *testing.T, c *testharness.Cluster, k, m uint8, what string) {
	t.Helper()
	gk, gm, _ := policy(t, c)
	if gk != k || gm != m {
		t.Fatalf("%s: policy %d+%d, want %d+%d", what, gk, gm, k, m)
	}
}

// The policy grows as devices join, waits while a device is asleep, and catches up
// when it wakes (docs/03 §Choosing the policy).
// Invariant: I-75
func TestCluster_AutoPolicy_GrowsOnJoin_DefersWhileAsleep(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0, testharness.AutoPolicy())
	defer c.Close()
	if !c.Leader().Node.FSM.Volume().Auto {
		t.Fatal("bootstrap did not turn auto mode on")
	}
	scanLeader(t, c)
	expect(t, c, 1, 0, "one device")
	c.AddNode(true)
	scanLeader(t, c)
	expect(t, c, 1, 1, "two voters")
	c.AddNode(true)
	scanLeader(t, c)
	expect(t, c, 2, 1, "three voters")
	if _, _, v := policy(t, c); v != 2 {
		t.Fatalf("policy version %d after two changes", v)
	}
	// a laptop joins (3 voters + 1 non-voter wants 2+2) but one voter is asleep: deferred
	c.AddNode(false)
	c.Kill(2)
	c.Clk.Advance(cluster.LiveThreshold + time.Second) // the prober declares it dead
	c.Probe()
	scanLeader(t, c)
	expect(t, c, 2, 1, "2+2 needs four live devices while one sleeps")
	c.Restart(2)
	c.Settle()
	c.Probe()
	scanLeader(t, c)
	expect(t, c, 2, 2, "everyone back")
	// data written along the way stays readable through the migrations
	c.WriteFile(0, "/f", testharness.Pattern(3*int(testharness.ChunkSize), 31))
	c.Settle()
	scanLeader(t, c)
	if got, err := c.ReadFileCold(1, "/f"); err != nil || len(got) != 3*int(testharness.ChunkSize) {
		t.Fatalf("read after migration: %v", err)
	}
}

// Membership is the only input: a dead device changes nothing, a removed one shrinks
// the policy once the survivors can place it.
// Invariant: I-75
func TestCluster_AutoPolicy_ShrinksOnRemoveNotOnDeath(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 2, testharness.AutoPolicy(), testharness.NonVoters(3))
	defer c.Close()
	scanLeader(t, c)
	expect(t, c, 2, 2, "three voters and a laptop")
	c.Kill(3)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	scanLeader(t, c)
	expect(t, c, 2, 2, "a dead laptop")
	if err := c.Leader().Node.MemberOp(context.Background(), rpc.MemberOp{Op: "remove", NodeIdx: c.Nodes[3].Idx()}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	c.Settle()
	scanLeader(t, c)
	expect(t, c, 2, 1, "laptop removed")
	// a demotion counts too: 2 voters + 1 laptop stays 2+1; promotion back is a no-op
	must(t, c.Leader().Node.MemberOp(context.Background(), rpc.MemberOp{Op: "set_voter", NodeIdx: c.Nodes[2].Idx(), Voter: false}))
	c.Settle()
	scanLeader(t, c)
	expect(t, c, 2, 1, "one demoted")
}

// Manual mode pins the policy; --auto hands it back.
// Invariant: I-75
func TestAutoPolicy_ManualPins(t *testing.T) {
	c := testharness.NewCluster(t, 3, 1, 0)
	defer c.Close()
	scanLeader(t, c)
	expect(t, c, 1, 0, "manual volume is left alone")
	ctx := context.Background()
	l := c.Leader()
	res, _, err := l.Node.Propose(ctx, meta.Command{Op: meta.OpSetVolumeMode, SetVolumeMode: &meta.SetVolumeMode{Auto: true, Now: c.Clk.Now().UnixNano()}})
	if err != nil || res.Err != nil {
		t.Fatalf("auto on: %v %v", err, res.Err)
	}
	scanLeader(t, c)
	expect(t, c, 2, 1, "auto applies the rule")
	for _, cmd := range []meta.Command{
		{Op: meta.OpSetVolumeMode, SetVolumeMode: &meta.SetVolumeMode{Auto: false, Now: c.Clk.Now().UnixNano()}},
		{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: 1, M: 1, Now: c.Clk.Now().UnixNano()}},
	} {
		if res, _, err := c.Leader().Node.Propose(ctx, cmd); err != nil || res.Err != nil {
			t.Fatalf("manual: %v %v", err, res.Err)
		}
	}
	scanLeader(t, c)
	expect(t, c, 1, 1, "pinned")
}
