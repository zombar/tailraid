package node

import (
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
)

// Growing a device in place with a pinned budget: raise node.capacity (or a disk's
// capacity) and restart. The store takes the new cap at once, and the maintenance tick
// registers it with the cluster so placement and status see it; a pinned value is exact,
// so no hysteresis applies.
// Invariant: I-74
func TestNode_Rebudget_PinnedChangeRegistered(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0)
	defer c.Close()
	nd := c.Nodes[0]
	ops := opsFor(c, nd)
	ops.pinned = true
	before, _ := nd.Node.FSM.Node(nd.Idx())
	for _, want := range []uint64{before.Capacity + 1, before.Capacity * 2, 0} {
		nd.Store.SetCapacity(int64(want))
		ops.rebudget(ctx)
		c.Settle()
		if got, _ := nd.Node.FSM.Node(nd.Idx()); got.Capacity != want {
			t.Fatalf("pinned %d: the cluster holds %d", want, got.Capacity)
		}
	}
	// unchanged: nothing is proposed (a proposal is applied before MemberOp returns)
	applied := nd.Node.AppliedIndex()
	ops.rebudget(ctx)
	if nd.Node.AppliedIndex() != applied {
		t.Fatal("an unchanged pinned budget was proposed again")
	}
}
