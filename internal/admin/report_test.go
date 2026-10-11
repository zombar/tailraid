package admin_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3/s3test"
	"github.com/zombar/tailraid/internal/testharness"
)

func TestReport_ReflectsClusterState(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/f", testharness.Pattern(2*int(testharness.ChunkSize), 1))
	c.Settle()
	leader := c.Leader()
	rep := admin.Build(leader.Node, leader.Prober, c.CID, c.Clk, nil)
	// three hosts, each with its one disk: six rows, the slots on the disks (docs/14 §9)
	if rep.Health != "healthy" || rep.Voters != 3 || rep.LiveVoters != 3 || len(rep.Nodes) != 6 || rep.Leader != leader.ID {
		t.Fatalf("report %+v", rep)
	}
	if rep.Volume.K != 2 || rep.Volume.Chunks != 2 || rep.Volume.Degraded != 0 || rep.Volume.LogicalBytes != 2*uint64(testharness.ChunkSize) {
		t.Fatalf("volume %+v", rep.Volume)
	}
	slots, devices, disks := 0, 0, 0
	for _, n := range rep.Nodes {
		slots += n.Slots
		if !n.Live || n.State != "active" {
			t.Fatalf("node %+v", n)
		}
		switch n.Kind {
		case "device":
			devices++
			if n.Slots != 0 || n.Capacity != 0 || n.Diskless {
				t.Fatalf("a device row with slots, a budget or flagged diskless: %+v", n)
			}
		case "disk":
			disks++
		}
	}
	if slots != 2*3 || devices != 3 || disks != 3 {
		t.Fatalf("slots %d, %d devices, %d disks", slots, devices, disks)
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
	rep = admin.Build(leader.Node, leader.Prober, c.CID, c.Clk, nil)
	// inside fill_after the chunks are under-replicated (degraded for the operator) but
	// nothing is repair-pending yet (docs/11 L-86); the node shows its missing slots
	if rep.LiveVoters != 2 || rep.Volume.Degraded != 2 || rep.Volume.RepairPending != 0 || rep.Volume.Unreadable != 0 {
		t.Fatalf("after kill %+v", rep.Volume)
	}
	found, diskFound := false, false
	for _, n := range rep.Nodes {
		if n.Idx == c.Nodes[victim].Idx() && !n.Live {
			found = true
		}
		if n.Idx == c.Nodes[victim].Target() && !n.Live { // the slots are on its disk (docs/14 §9)
			diskFound = true
			if n.Missing != 2 {
				t.Fatalf("dead disk missing slots %d, want 2", n.Missing)
			}
		}
	}
	if !found || !diskFound || len(rep.Warnings) == 0 {
		t.Fatalf("dead node and its disk not reported: %+v", rep)
	}
	// past fill_after the slots are bad (L-87): repair pending as well
	c.Clk.Advance(cluster.FillAfter)
	c.Probe()
	rep = admin.Build(c.Leader().Node, c.Leader().Prober, c.CID, c.Clk, nil)
	if rep.Volume.Degraded != 2 || rep.Volume.RepairPending != 2 || rep.Volume.Unreadable != 0 {
		t.Fatalf("past fill_after %+v", rep.Volume)
	}
}

// Past evict_after the node is marked lost and the warning names the command that drops
// its slots so repair can re-place them.
// Lifecycle: L-92
func TestStatus_SuggestsRemoveAfterEvict(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/f", testharness.Pattern(int(testharness.ChunkSize), 2))
	c.Settle()
	leader := c.Leader()
	victim := (leader.I + 1) % 3
	c.Kill(victim)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	c.Clk.Advance(cluster.EvictAfter)
	c.Probe()
	rep := admin.Build(leader.Node, leader.Prober, c.CID, c.Clk, nil)
	var lost *admin.NodeReport
	for i := range rep.Nodes {
		if rep.Nodes[i].Idx == c.Nodes[victim].Idx() {
			lost = &rep.Nodes[i]
		}
	}
	if lost == nil || !lost.Lost || lost.Live {
		t.Fatalf("victim not marked lost: %+v", lost)
	}
	want := "node remove " + strconv.Itoa(int(c.Nodes[victim].Idx())) + " --force"
	for _, w := range rep.Warnings {
		if strings.Contains(w, want) {
			return
		}
	}
	t.Fatalf("no warning naming %q: %v", want, rep.Warnings)
}

// A bucket's row carries what it stores, the shard bytes of the slots the placement
// table gives it, and no free space (its probe is a HEAD); every disk's row carries the
// same figure next to its filesystem's free bytes, and a device's is zero.
// Invariant: I-178
func TestReport_S3NodeStoredBytes(t *testing.T) {
	c := testharness.NewCluster(t, 2, 2, 1)
	defer c.Close()
	srv := s3test.New()
	defer srv.Close()
	bucket := c.AddS3(srv, "offsite", "c")
	// three chunks, the last one a single byte: its shard is shorter than the others'
	c.WriteFile(0, "/f", testharness.Pattern(2*int(testharness.ChunkSize)+1, 1))
	c.Settle()
	leader := c.Leader()
	rep := admin.Build(leader.Node, leader.Prober, c.CID, c.Clk, nil)

	// the oracle walks the placement table itself
	stored, slots := map[meta.NodeIdx]uint64{}, map[meta.NodeIdx]int{}
	leader.Node.FSM.ForEachChunk(func(ch meta.Chunk) bool {
		for _, s := range ch.Placement {
			if s != 0 {
				stored[s] += uint64(ec.ShardLen(int(ch.Size), int(ch.K)))
				slots[s]++
			}
		}
		return true
	})
	if slots[bucket] == 0 {
		t.Fatalf("no stripe placed a slot on the bucket: %+v", slots)
	}
	seen := false
	for _, n := range rep.Nodes {
		if n.Stored != stored[n.Idx] || n.Slots != slots[n.Idx] {
			t.Fatalf("%s (%s): stored %d slots %d, want stored %d slots %d", n.Name, n.Kind, n.Stored, n.Slots, stored[n.Idx], slots[n.Idx])
		}
		switch n.Kind {
		case "s3":
			seen = true
			if n.S3 == nil || n.Free != 0 || n.Stored == 0 || n.Stored >= n.Capacity || n.Version != "" || n.Applied != 0 || n.Addr != "" {
				t.Fatalf("bucket row %+v", n)
			}
		case "device":
			if n.Stored != 0 {
				t.Fatalf("a device stores nothing: %+v", n)
			}
		case "disk":
			if n.Stored == 0 || n.Free <= 0 {
				t.Fatalf("disk row %+v", n)
			}
		}
	}
	if !seen {
		t.Fatalf("no bucket row: %+v", rep.Nodes)
	}
	b, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(b), `"stored_bytes"`) {
		t.Fatalf("stored_bytes missing from the JSON: %v", err)
	}
}
