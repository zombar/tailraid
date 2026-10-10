package cluster_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
)

// A leader that shuts down hands leadership over first: another node leads as soon as
// Shutdown returns (the transfer future resolves when the old leader has stepped down,
// which is a vote's round trip before the target holds the term, so the check waits
// for that and not for an election timeout), and writes through it land.
//
// Invariant: I-123
func TestCluster_ShutdownTransfersLeadership(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/before", testharness.Pattern(100, 1))
	c.Settle()
	leader := c.Leader()
	start := time.Now()
	must(t, leader.Node.Shutdown(5*time.Second))
	newLeader := -1
	testharness.Eventually(t, func() bool {
		for i, nd := range c.Nodes {
			if nd != leader && nd.Node.IsLeader() {
				newLeader = i
				return true
			}
		}
		return false
	}, "no other node leads after the hand-over")
	// the harness's election timeout is what a transfer saves; the hand-over must be
	// well inside it
	if took, limit := time.Since(start), testharness.RaftTiming().Election; took > limit {
		t.Fatalf("the hand-over took %v, longer than an election timeout (%v)", took, limit)
	}
	c.Kill(leader.I) // the rest of the harness teardown for that node
	other := 3 - leader.I - newLeader
	data := testharness.Pattern(200, 2)
	c.WriteFile(newLeader, "/after", data)
	c.Settle()
	if got, err := c.ReadFileCold(other, "/after"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("write through the new leader: %v", err)
	}
}

// A node is ready while it knows a leader and has applied what Raft committed: an
// isolated follower stops being ready, is ready again once healed, and a closed node
// never is.
//
// Invariant: I-125
func TestCluster_Ready_FollowsLeaderContact(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/f", testharness.Pattern(10, 1))
	c.Settle()
	for i, nd := range c.Nodes {
		if !nd.Node.Ready() {
			t.Fatalf("n%d not ready after settle", i)
		}
	}
	leader := c.Leader()
	f := (leader.I + 1) % 3
	others := []int{(f + 1) % 3, (f + 2) % 3}
	c.Partition([]int{f}, others)
	testharness.Eventually(t, func() bool { return !c.Nodes[f].Node.Ready() }, "isolated follower loses readiness")
	c.Heal()
	testharness.Eventually(t, func() bool { return c.Nodes[f].Node.Ready() }, "healed follower is ready again")
	c.Kill(f)
	if c.Nodes[f].Node.Ready() {
		t.Fatal("a closed node reports ready")
	}
}
