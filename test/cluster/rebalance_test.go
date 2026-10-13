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

// A large disk added to a host whose small disk holds every slot: the slots whose home is
// the new disk become away, the gate opens at the end of the pass that finds them (none
// move in it), and the following passes move them home, at most HomeBatch a pass, until
// every chunk is home; every chunk reads exact throughout, no slot leaves its host (host
// 1's disk is far larger but alone on its host), no chunk gets two slots on one target,
// and a further pass moves nothing.
// Invariant: I-172, I-294, I-295, I-65, I-9
// Lifecycle: L-211
func TestRepair_Home_AddDiskFillsWithinHost(t *testing.T) {
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
	cfg := repairCfg()
	cfg.HomeBatch = 4
	r := newRepairer(c, c.Leader(), cfg)
	st, err := r.Scan(ctx)
	must(t, err)
	if st.Homed != 0 || st.AwaySlots != 0 || r.Home().Open {
		t.Fatalf("every chunk is home before the disk joins: %+v, gate %+v", st, r.Home())
	}

	large := c.AddDiskCap(0, 10<<30)
	st, err = r.Scan(ctx)
	must(t, err)
	if st.Homed != 0 || st.MovableBytes == 0 || !r.Home().Open {
		t.Fatalf("the pass that finds the away slots opens the gate and moves none: %+v, gate %+v", st, r.Home())
	}
	passes := 0
	for ; passes < 10; passes++ {
		st, err = r.Scan(ctx)
		must(t, err)
		if st.Homed == 0 {
			break
		}
		if st.Homed > cfg.HomeBatch {
			t.Fatalf("pass %d moved %d chunks, the batch is %d", passes, st.Homed, cfg.HomeBatch)
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
	if st.AwaySlots != 0 || st.MovableBytes != 0 || r.Home().Open {
		t.Fatalf("after %d passes: %+v, gate %+v", passes, st, r.Home())
	}
	// home is weighted by capacity: the large disk (10×) holds most of the host's slots
	if s, l := slotsOn(c, 0, "/f", small), slotsOn(c, 0, "/f", large); s+l != chunks || l < chunks/2 {
		t.Fatalf("small disk %d slots, large %d after %d passes", s, l, passes)
	}
	if passes < 2 {
		t.Fatalf("%d passes moved the slots with a batch of 4", passes)
	}
	st, err = r.Scan(ctx)
	must(t, err)
	if st.Homed != 0 {
		t.Fatalf("a converged volume moved %d chunks", st.Homed)
	}
	got, err := c.ReadFileCold(1, "/f")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("final read: %v", err)
	}
}
