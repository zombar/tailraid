package cluster_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/rpc"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// Two always-on voters and two laptops (non-voters) on 2+2: every chunk's data slots sit
// on the voters, so the volume stays readable with both laptops asleep (docs/03
// §Voter-first slots).
// Invariant: I-77
func TestCluster_ReadableWithNonVotersAsleep(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 2, testharness.NonVoters(2, 3))
	defer c.Close()
	data := testharness.Pattern(5*int(testharness.ChunkSize)+123, 21)
	c.WriteFile(0, "/f", data)
	c.WriteFile(1, "/g", testharness.Pattern(3*int(testharness.ChunkSize), 22)) // from the other voter too
	c.Settle()
	voters := map[meta.NodeIdx]bool{}
	for i, nd := range c.Nodes {
		if c.Voter(i) {
			voters[nd.Target()] = true // the slots are on the voter's disk (docs/14 §5)
		}
	}
	chunks := 0
	c.Nodes[0].Node.FSM.ForEachChunk(func(ch meta.Chunk) bool {
		chunks++
		for i := range ch.K {
			if n := ch.Placement[i]; !voters[n] {
				t.Fatalf("chunk %x data slot %d on non-voter %d (placement %v)", ch.Hash[:4], i, n, ch.Placement)
			}
		}
		return true
	})
	if chunks < 8 {
		t.Fatalf("only %d chunks committed", chunks)
	}
	// both laptops asleep: reads from either voter still succeed, cold cache included
	c.Kill(2)
	c.Kill(3)
	c.Probe()
	for _, gw := range []int{0, 1} {
		got, err := c.ReadFileCold(gw, "/f")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read /f from voter n%d with laptops asleep: %v", gw, err)
		}
	}
	// and a write still commits with k acks from the voters (degraded parity, docs/00)
	c.WriteFile(0, "/h", []byte("written with the laptops closed"))
	if got, err := c.ReadFileCold(0, "/h"); err != nil || string(got) != "written with the laptops closed" {
		t.Fatalf("read /h: %q %v", got, err)
	}
}

// suffrage is the Raft suffrage of node id in nd's configuration ("" when absent).
func suffrage(t *testing.T, nd *testharness.ClusterNode, id string) string {
	t.Helper()
	f := nd.Node.Raft.GetConfiguration()
	must(t, f.Error())
	for _, s := range f.Configuration().Servers {
		if string(s.ID) == id {
			return s.Suffrage.String()
		}
	}
	return ""
}

// promote and demote change the Raft vote as well as the row: demote of a voter is
// DemoteVoter (raft's AddNonvoter leaves an existing voter's vote alone), and a leader
// that demotes itself records the row before it hands leadership over (docs/02
// §Lifecycle commands).
// Invariant: I-145
func TestCluster_DemotePromote_RaftFollowsRow(t *testing.T) {
	c := testharness.NewCluster(t, 3, 1, 0)
	defer c.Close()
	setVoter := func(nd *testharness.ClusterNode, voter bool) {
		t.Helper()
		var err error
		for range 20 { // a leader that just demoted itself is gone: ask the new one
			if err = c.Leader().Node.MemberOp(context.Background(), rpc.MemberOp{Op: "set_voter", NodeIdx: nd.Idx(), Voter: voter}); err == nil {
				break
			}
			<-time.After(100 * time.Millisecond)
		}
		must(t, err)
		c.Settle()
	}
	agree := func(nd *testharness.ClusterNode, want bool) {
		t.Helper()
		leader := c.Leader()
		row, _ := leader.Node.FSM.Node(nd.Idx())
		wantS := "Nonvoter"
		if want {
			wantS = "Voter"
		}
		if got := suffrage(t, leader, nd.ID); row.Voter != want || got != wantS {
			t.Fatalf("%s: row voter %v, raft %s; want %v / %s", nd.ID, row.Voter, got, want, wantS)
		}
	}
	var follower *testharness.ClusterNode
	for _, nd := range c.Nodes {
		if !nd.Node.IsLeader() {
			follower = nd
		}
	}
	setVoter(follower, false)
	agree(follower, false)
	setVoter(follower, true)
	agree(follower, true)
	// the leader demotes itself: a laptop that led the cluster (the case that found this)
	old := c.Leader()
	setVoter(old, false)
	agree(old, false)
	if c.Leader() == old {
		t.Fatal("a demoted leader still leads")
	}
}
