package cluster_test

import (
	"bytes"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// One machine in auto mode: adding a disk grows the policy to mirror onto it; draining the
// disk (one of only two targets) shrinks the policy back, the repairer migrates every
// chunk off it, and it can then be removed, with the data exact throughout (docs/14 §6).
// Invariant: I-88, I-42
func TestCluster_AutoPolicy_AddDrainRemoveDisk(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0, testharness.AutoPolicy())
	defer c.Close()
	data := testharness.Pattern(3*int(testharness.ChunkSize), 8)
	c.WriteFile(0, "/f", data)
	pass := func() {
		t.Helper()
		for i := 0; i < 4; i++ {
			_, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
			must(t, err)
			c.Settle()
		}
	}
	vol := func() meta.Volume { return c.Nodes[0].Node.FSM.Volume() }
	disk := c.AddDisk(0)
	pass()
	if v := vol(); v.K != 1 || v.M != 1 {
		t.Fatalf("after add-disk: %d+%d, want 1+1", v.K, v.M)
	}
	for _, ch := range c.ChunksOf(0, "/f") {
		if ch.K != 1 || ch.M != 1 || len(ch.Placement) != 2 || ch.Placement[0] == 0 || ch.Placement[1] == 0 {
			t.Fatalf("chunk not mirrored: %d+%d %v", ch.K, ch.M, ch.Placement)
		}
	}
	// drain the disk: one target left, so the policy must shrink to free it
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: disk, State: meta.Draining}))
	c.Settle()
	pass()
	if v := vol(); v.K != 1 || v.M != 0 {
		t.Fatalf("after draining the disk: %d+%d, want 1+0", v.K, v.M)
	}
	for _, ch := range c.ChunksOf(0, "/f") {
		for _, s := range ch.Placement {
			if s == disk {
				t.Fatalf("drained disk still placed: %v", ch.Placement)
			}
		}
	}
	got, err := c.ReadFileCold(0, "/f")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after drain: %v", err)
	}
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: disk}))
	c.Settle()
	if nd, _ := c.Nodes[0].Node.FSM.Node(disk); nd.State != meta.Removed {
		t.Fatalf("disk state %v after remove", nd.State)
	}
}
