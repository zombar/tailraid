package admin_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/testharness"
)

func TestReport_ReflectsClusterState(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/f", testharness.Pattern(2*int(testharness.ChunkSize), 1))
	c.Settle()
	leader := c.Leader()
	rep := admin.Build(leader.Node, leader.Prober, c.CID, c.Clk)
	if rep.Health != "healthy" || rep.Voters != 3 || rep.LiveVoters != 3 || len(rep.Nodes) != 3 || rep.Leader != leader.ID {
		t.Fatalf("report %+v", rep)
	}
	if rep.Volume.K != 2 || rep.Volume.Chunks != 2 || rep.Volume.Degraded != 0 || rep.Volume.LogicalBytes != 2*uint64(testharness.ChunkSize) {
		t.Fatalf("volume %+v", rep.Volume)
	}
	slots := 0
	for _, n := range rep.Nodes {
		slots += n.Slots
		if !n.Live || n.State != "active" || n.Kind != "device" {
			t.Fatalf("node %+v", n)
		}
	}
	if slots != 2*3 {
		t.Fatalf("slots %d", slots)
	}
	if len(rep.Warnings) != 0 {
		t.Fatalf("warnings on a healthy cluster: %v", rep.Warnings)
	}
	// JSON round trip keeps the shape used by the CLI
	b, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(b), `"nodes"`) {
		t.Fatal(err)
	}

	// kill a node: it goes dead, chunks become degraded, a warning appears
	victim := 2
	if leader.I == 2 {
		victim = 1
	}
	c.Kill(victim)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	leader = c.Leader()
	rep = admin.Build(leader.Node, leader.Prober, c.CID, c.Clk)
	if rep.LiveVoters != 2 || rep.Volume.Degraded != 2 || rep.Volume.Unreadable != 0 {
		t.Fatalf("after kill %+v", rep.Volume)
	}
	found := false
	for _, n := range rep.Nodes {
		if n.Idx == c.Nodes[victim].Idx() && !n.Live {
			found = true
		}
	}
	if !found || len(rep.Warnings) == 0 {
		t.Fatalf("dead node not reported: %+v", rep)
	}
}
