package admin_test

import (
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// status lists disks under their host, reports what the policy survives, and counts the
// chunks above the host cap (docs/14 §8).
// Invariant: I-88, I-89
func TestReport_DisksHostToleranceAndSkew(t *testing.T) {
	c := testharness.NewCluster(t, 2, 2, 1)
	defer c.Close()
	d1 := c.AddDisk(0)
	c.AddDisk(0)
	leader := c.Leader()
	host0, host1 := c.Nodes[0].Idx(), c.Nodes[1].Idx()
	// chunks written while host 1 drains all land on host 0: skewed once it returns
	if err := leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: host1, State: meta.Draining}); err != nil {
		t.Fatal(err)
	}
	c.Settle()
	c.WriteFile(0, "/s", testharness.Pattern(2*int(testharness.ChunkSize), 1))
	if err := c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: host1, State: meta.Active}); err != nil {
		t.Fatal(err)
	}
	c.Settle()
	c.Probe()
	nd := c.Nodes[0]
	rep := admin.Build(nd.Node, nd.Prober, c.CID, c.Clk, nil)
	var disk admin.NodeReport
	for _, n := range rep.Nodes {
		if n.Idx == d1 {
			disk = n
		}
	}
	if disk.Kind != "disk" || disk.Host != host0 || !disk.Self || !disk.Live || disk.Slots == 0 {
		t.Fatalf("disk row %+v", disk)
	}
	// 2 hosts (3 targets + 1) at 2+1: one host always holds 2 of the 3 slots, so the
	// pinned policy survives 1 disk and no host
	if rep.Volume.ToleratesDisks != 1 || rep.Volume.ToleratesHosts != 0 {
		t.Fatalf("tolerance %d disks %d hosts", rep.Volume.ToleratesDisks, rep.Volume.ToleratesHosts)
	}
	if rep.Volume.DomainSkew != 2 || rep.Volume.Degraded != 0 {
		t.Fatalf("domain skew %d (degraded %d), want 2 skewed chunks", rep.Volume.DomainSkew, rep.Volume.Degraded)
	}
}
