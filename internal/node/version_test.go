package node

import (
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// At start a node whose row names another version than the binary it runs reports its
// own through the leader (a follower forwards), after which every node's table shows it;
// a row that already matches is left alone, and the other rows are untouched (docs/02).
// Invariant: I-133
func TestReportVersion_UpdatesOwnRow(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := c.Leader()
	follower := c.Nodes[(leader.I+1)%3]
	for _, nd := range []*testharness.ClusterNode{follower, leader} {
		ok, err := reportVersion(ctx, nd.Node, nd.Client, nd.ID, nd.Idx(), "v9")
		if err != nil || !ok {
			t.Fatalf("n%d: reported=%v err=%v", nd.I, ok, err)
		}
	}
	c.Settle()
	for _, nd := range c.Nodes {
		rows := map[meta.NodeIdx]string{}
		for _, row := range nd.Node.FSM.Nodes() {
			if row.Kind == meta.KindDevice {
				rows[row.Idx] = row.Version
			}
		}
		third := c.Nodes[(leader.I+2)%3]
		if rows[follower.Idx()] != "v9" || rows[leader.Idx()] != "v9" || rows[third.Idx()] != "harness" {
			t.Fatalf("n%d sees versions %v", nd.I, rows)
		}
	}
	// a matching row is reported without a proposal: the applied index stays put
	idx := leader.Node.AppliedIndex()
	if ok, err := reportVersion(ctx, leader.Node, leader.Client, leader.ID, leader.Idx(), "v9"); err != nil || !ok {
		t.Fatalf("second report: %v %v", ok, err)
	}
	if got := leader.Node.AppliedIndex(); got != idx {
		t.Fatalf("a matching row was rewritten: applied %d -> %d", idx, got)
	}
}
