package cluster

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// The join's member seed round-trips through the data directory, keeps only device rows
// with an address, is absent without error before a join, and is gone after Forget.
//
// Invariant: I-92
func TestMemberSeed_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadMemberSeed(dir); err != nil || got != nil {
		t.Fatalf("before a join: %v %v", got, err)
	}
	members := []meta.Node{
		{Idx: 1, ID: "leader", Kind: meta.KindDevice, Addr: "100.64.0.11", TSStableID: "ts-a", State: meta.Active},
		{Idx: 2, ID: "disk", Kind: meta.KindDisk, Host: 1},
		{Idx: 3, ID: "gone", Kind: meta.KindDevice, Addr: "100.64.0.9", State: meta.Removed},
		{Idx: 4, ID: "noaddr", Kind: meta.KindDevice},
	}
	if err := WriteMemberSeed(dir, members); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMemberSeed(dir)
	if err != nil || len(got) != 1 || got[0].ID != "leader" || got[0].TSStableID != "ts-a" {
		t.Fatalf("seed: %+v %v", got, err)
	}
	if err := ForgetMemberSeed(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, seedFile)); err == nil {
		t.Fatal("seed file still there after Forget")
	}
	if err := ForgetMemberSeed(dir); err != nil {
		t.Fatalf("forgetting twice: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, seedFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMemberSeed(dir); err == nil {
		t.Fatal("a corrupt seed must be an error, not an empty table")
	}
}

// A node with neither a table nor a seed rebuilds the members from the entries its own
// log holds: the replay yields exactly the device rows with an address that the live
// table has, after an address change, a disk, a join and a removed member, so the leader
// the log names is accepted (docs/08 §Join).
//
// Invariant: I-134
func TestMembersFromLog_MatchesTable(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	self, _ := n.SelfIdx()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(n.MemberOp(ctx, rpc.MemberOp{Op: "update_addr", NodeIdx: self, Addr: "100.64.0.9"}))
	must(n.MemberOp(ctx, rpc.MemberOp{Op: "add_disk", NodeIdx: self, DiskID: "abcd0123abcd0123", Name: "solo/1", Capacity: 7}))
	join := func(id, addr string) meta.NodeIdx {
		resp, err := n.Join(ctx, rpc.JoinRequest{NodeID: id, Name: id[:4], Addr: addr, TSStableID: "ts-" + id[:4], RaftPort: 1, RPCPort: 2})
		must(err)
		return resp.NodeIdx
	}
	join("aaaa000000000001", "100.64.0.21")
	gone := join("bbbb000000000002", "100.64.0.22")
	must(n.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: gone}))

	got, err := n.StartSeed(meta.NewFSM(), nil) // no table restored, no seed file
	must(err)
	var want []meta.Node
	for _, m := range n.FSM.Nodes() {
		if m.Kind == meta.KindDevice && m.State != meta.Removed && m.Addr != "" {
			want = append(want, m)
		}
	}
	if len(want) != 2 || len(got) != len(want) {
		t.Fatalf("replayed %d members, table has %d (want 2):\n%+v\n%+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].Idx != want[i].Idx || got[i].ID != want[i].ID || got[i].Addr != want[i].Addr || got[i].TSStableID != want[i].TSStableID {
			t.Errorf("member %d: replayed %+v, table %+v", i, got[i], want[i])
		}
	}
}

// A node restarted after members joined, whose seed file and restored table predate the
// joins, still accepts them: they are in its own log. Before, serve consulted the log
// only with no seed file and an empty table, and refused the leader forever (soak 23.9
// on the compose rig, 2026-10-13: n1 restarted a minute after the joins).
// Invariant: I-281
func TestStartSeed_NamesMembersJoinedAfterTheSnapshot(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	join := func(id, addr string) meta.NodeIdx {
		resp, err := n.Join(ctx, rpc.JoinRequest{NodeID: id, Name: id[:4], Addr: addr, TSStableID: "ts-" + id[:4], RaftPort: 1, RPCPort: 2})
		must(err)
		return resp.NodeIdx
	}
	self, _ := n.SelfIdx()
	must(n.MemberOp(ctx, rpc.MemberOp{Op: "update_addr", NodeIdx: self, Addr: "100.64.0.9"}))
	gone := join("cccc000000000003", "100.64.0.23")
	// what a restart restores: the state as of now, and the seed serve wrote from it
	restored, err := cloneFSM(n.FSM)
	must(err)
	file := devices(restored.Nodes())
	if len(file) != 2 {
		t.Fatalf("seed file %+v", file)
	}
	// after the snapshot, two members join and one leaves; the log holds it all
	join("aaaa000000000001", "100.64.0.21")
	join("bbbb000000000002", "100.64.0.22")
	must(n.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: gone}))

	got, err := n.StartSeed(restored, file)
	must(err)
	addrs := map[string]bool{}
	for _, m := range got {
		addrs[m.Addr] = true
	}
	for _, want := range []string{"100.64.0.9", "100.64.0.21", "100.64.0.22"} {
		if !addrs[want] {
			t.Errorf("seed %v lacks %s", addrs, want)
		}
	}
	if addrs["100.64.0.23"] {
		t.Errorf("seed %v keeps the member the log removed", addrs)
	}
}

func devices(nodes []meta.Node) []meta.Node {
	var out []meta.Node
	for i := range nodes {
		if m := &nodes[i]; m.Kind == meta.KindDevice && m.State != meta.Removed && m.Addr != "" {
			out = append(out, *m)
		}
	}
	return out
}
