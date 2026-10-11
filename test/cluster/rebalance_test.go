package cluster_test

import (
	"bytes"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// slotsOn counts the slots of path's chunks on target s in node i's view.
func slotsOn(c *testharness.Cluster, i int, path string, s meta.NodeIdx) int {
	n := 0
	for _, ch := range c.ChunksOf(i, path) {
		for _, p := range ch.Placement {
			if p == s {
				n++
			}
		}
	}
	return n
}

// A large disk added to a host whose small disk holds every slot: repair passes move
// slots to the new disk, at most RebalanceBatch a pass, until the small disk is within
// the skew of its share; every chunk reads exact throughout, no slot leaves the host,
// no chunk gets two slots on one target, and a host with one disk is never a source or
// a destination across hosts.
// Invariant: I-172, I-65, I-9
// Lifecycle: L-211
func TestRepair_Rebalance_MovesWithinHost(t *testing.T) {
	c := testharness.NewCluster(t, 2, 1, 1) // two slots per chunk, one per host
	defer c.Close()
	small := c.Nodes[0].Target()
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_capacity", NodeIdx: small, Capacity: 1 << 30}))
	c.Settle()
	const chunks = 20
	data := testharness.Pattern(chunks*int(testharness.ChunkSize), 5)
	c.WriteFile(0, "/f", data)
	if n := slotsOn(c, 0, "/f", small); n != chunks {
		t.Fatalf("setup: %d of %d chunks on host 0's small disk", n, chunks)
	}
	// host 1's disk is far larger than host 0's, but it is alone on its host: nothing
	// moves between hosts for capacity
	cfg := repairCfg()
	cfg.RebalanceBatch = 4
	st, err := newRepairer(c, c.Leader(), cfg).Scan(ctx)
	must(t, err)
	if st.Rebalanced != 0 {
		t.Fatalf("moved %d slots across hosts", st.Rebalanced)
	}

	large := c.AddDiskCap(0, 10<<30)
	passes := 0
	for ; passes < 10; passes++ {
		st, err = newRepairer(c, c.Leader(), cfg).Scan(ctx)
		must(t, err)
		if st.Rebalanced == 0 {
			break
		}
		if st.Rebalanced > cfg.RebalanceBatch {
			t.Fatalf("pass %d moved %d slots, the batch is %d", passes, st.Rebalanced, cfg.RebalanceBatch)
		}
		got, err := c.ReadFileCold(1, "/f")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read after pass %d: %v", passes, err)
		}
		for _, ch := range c.ChunksOf(0, "/f") {
			if ph := perHost(c, 0, ch); ph[c.Nodes[0].Idx()] != 1 || ph[c.Nodes[1].Idx()] != 1 {
				t.Fatalf("after pass %d: placement %v by host %v", passes, ch.Placement, ph)
			}
			if ch.Placement[0] == ch.Placement[1] {
				t.Fatalf("after pass %d: two slots on one target %v", passes, ch.Placement)
			}
		}
	}
	// shares within host 0: 20 × 1/11 ≈ 1.8 on the small disk, the rest on the large one
	if n := slotsOn(c, 0, "/f", small); n > 2 {
		t.Fatalf("small disk still holds %d slots after %d passes", n, passes)
	}
	if n := slotsOn(c, 0, "/f", large); n < chunks-2 {
		t.Fatalf("large disk holds %d slots after %d passes", n, passes)
	}
	if passes < 4 {
		t.Fatalf("%d passes moved 18 slots with a batch of 4", passes)
	}
	// a further pass finds nothing to do
	st, err = newRepairer(c, c.Leader(), cfg).Scan(ctx)
	must(t, err)
	if st.Rebalanced != 0 {
		t.Fatalf("a balanced host moved %d slots", st.Rebalanced)
	}
	got, err := c.ReadFileCold(1, "/f")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("final read: %v", err)
	}
}
