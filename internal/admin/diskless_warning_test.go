package admin_test

import (
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/testharness"
)

// A dead diskless host gets no "unreachable" warning, neither the plain one nor the
// `node remove --force` one after evict_after: it holds no slot, so there is nothing
// to re-place and nothing the operator need do; a dead disk host still warns.
//
// Invariant: I-118
func TestStatus_NoUnreachableWarningForDisklessHost(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	bare := c.AddDisklessNode(false)
	c.WriteFile(0, "/f", testharness.Pattern(int(testharness.ChunkSize), 2))
	c.Settle()
	leader := c.Leader()
	victim := (leader.I + 1) % 3
	c.Kill(bare)
	c.Kill(victim)
	for _, step := range []struct {
		name    string
		advance time.Duration
	}{{"dead", cluster.LiveThreshold + time.Second}, {"lost", cluster.EvictAfter}} {
		c.Clk.Advance(step.advance)
		c.Probe()
		rep := admin.Build(leader.Node, leader.Prober, c.CID, c.Clk, nil)
		var bareRow, victimRow *admin.NodeReport
		for i := range rep.Nodes {
			switch rep.Nodes[i].Idx {
			case c.Nodes[bare].Idx():
				bareRow = &rep.Nodes[i]
			case c.Nodes[victim].Idx():
				victimRow = &rep.Nodes[i]
			}
		}
		if bareRow == nil || bareRow.Live || !bareRow.Diskless || bareRow.DeadFor == "" {
			t.Fatalf("%s: diskless row %+v", step.name, bareRow)
		}
		named := func(n *admin.NodeReport) bool {
			for _, w := range rep.Warnings {
				if strings.Contains(w, "node "+n.Name+" has been unreachable") {
					return true
				}
			}
			return false
		}
		if named(bareRow) {
			t.Fatalf("%s: the diskless host is warned about: %v", step.name, rep.Warnings)
		}
		if victimRow == nil || !named(victimRow) {
			t.Fatalf("%s: the dead disk host is not warned about: %v", step.name, rep.Warnings)
		}
	}
}
