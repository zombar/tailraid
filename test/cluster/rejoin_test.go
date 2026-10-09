package cluster_test

import (
	"bytes"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A device that was reset joins again from its old Raft address under a new node ID:
// the leader replaces the stale server and re-keys the device's row to the new ID (its
// disk stays its disk) instead of refusing the address, and the node serves again.
//
// Invariant: I-94
func TestCluster_RejoinAfterReset_ReplacesOldServer(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	i := c.AddNode(false)
	oldID, oldIdx := c.Nodes[i].ID, c.Nodes[i].Idx()
	c.WriteFile(0, "/before", testharness.Pattern(100, 1))
	c.Settle()
	c.ResetAndRejoin(i)
	if c.Nodes[i].ID == oldID {
		t.Fatal("the reset kept the node ID")
	}
	leader := c.Leader()
	servers := leader.Node.Raft.GetConfiguration().Configuration().Servers
	seen := map[raft.ServerID]int{}
	for _, s := range servers {
		if s.ID == raft.ServerID(oldID) {
			t.Fatalf("the stale server %s is still in the configuration", oldID)
		}
		seen[s.ID]++
	}
	if seen[raft.ServerID(c.Nodes[i].ID)] != 1 {
		t.Fatalf("the re-joined node is not in the configuration once: %+v", servers)
	}
	row, ok := leader.Node.FSM.Node(oldIdx)
	if !ok || row.ID != c.Nodes[i].ID || row.State != meta.Active {
		t.Fatalf("row %d after the re-join: %+v (want the new ID, active)", oldIdx, row)
	}
	if _, ok := leader.Node.FSM.NodeByID(oldID); ok {
		t.Fatalf("a row still carries the old ID %s", oldID)
	}
	disks := 0
	for _, nd := range leader.Node.FSM.Nodes() {
		if nd.Kind == meta.KindDisk && nd.Host == oldIdx && nd.State == meta.Active {
			disks++
		}
	}
	if disks != 1 {
		t.Fatalf("%d active disks under the re-joined host, want its one disk", disks)
	}
	data := testharness.Pattern(200, 2)
	c.WriteFile(i, "/after", data)
	c.Settle()
	if got, err := c.ReadFileCold(0, "/after"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("write through the re-joined node: %v", err)
	}
	if got, err := c.ReadFileCold(i, "/before"); err != nil || !bytes.Equal(got, testharness.Pattern(100, 1)) {
		t.Fatalf("the re-joined node reads the old file: %v", err)
	}
}
