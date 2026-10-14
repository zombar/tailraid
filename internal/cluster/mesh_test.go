package cluster

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

func meshKey(b byte) meta.MeshKey {
	var k meta.MeshKey
	for i := range k {
		k[i] = b + byte(i)
	}
	return k
}

func keyText(k meta.MeshKey) string { return base64.StdEncoding.EncodeToString(k[:]) }

// bootstrapMesh is bootstrap with a cluster subnet and the first node at its first host.
func bootstrapMesh(t *testing.T, dir string) *Node {
	t.Helper()
	o := opts(t, dir)
	o.MeshSubnet = "100.70.3.0/24"
	n, err := Bootstrap(ctx, o, meta.Node{Name: "solo", Kind: meta.KindDevice, Addr: "100.70.3.1", RaftPort: 1, RPCPort: 2, Key: meshKey(1), MeshPort: 20483},
		meta.SetVolume{ChunkSize: 1 << 20, K: 1, M: 0, Now: 1}, false, "token")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// An introduce gives a key an address as a pending row, the same key the same address,
// the join from it completes the row and the pending row goes, a key never introduced
// cannot join, a member publishes its candidates, a device reset joins again with its
// key under a new node ID and keeps its row, and pending rows expire.
//
// Invariant: I-319, I-320, I-321, I-322
func TestMesh_IntroduceJoinExpire(t *testing.T) {
	n := bootstrapMesh(t, t.TempDir())
	defer n.Close()
	if m := n.FSM.Meta(); m.MeshSubnet != "100.70.3.0/24" || m.NextMeshHost != 2 {
		t.Fatalf("meta %+v", m)
	}
	k2, k3 := meshKey(2), meshKey(3)
	addr, err := n.Introduce(ctx, k2)
	if err != nil || addr.String() != "100.70.3.2/24" {
		t.Fatalf("introduce: %v %v", addr, err)
	}
	if again, err := n.Introduce(ctx, k2); err != nil || again != addr {
		t.Fatalf("introduce again: %v %v", again, err)
	}
	if _, err := n.Introduce(ctx, meta.MeshKey{}); !errors.Is(err, ErrBadMemberOp) {
		t.Fatalf("a zero key: %v", err)
	}
	if p, ok := n.FSM.PendingByAddr("100.70.3.2"); !ok || p.Key != k2 {
		t.Fatalf("pending at the address: %+v %v", p, ok)
	}
	// a join by a key never introduced, and one that names another address
	if _, err := n.Join(ctx, rpc.JoinRequest{NodeID: "0000000000000003", Name: "three", Key: keyText(k3), RaftPort: 1, RPCPort: 2}); !errors.Is(err, ErrNoIntroduce) {
		t.Fatalf("join without an introduce: %v", err)
	}
	if _, err := n.Join(ctx, rpc.JoinRequest{NodeID: "0000000000000002", Name: "two", Key: keyText(k2), Addr: "100.70.3.9", RaftPort: 1, RPCPort: 2}); !errors.Is(err, ErrBadMemberOp) {
		t.Fatalf("join from another address: %v", err)
	}
	if _, err := n.Join(ctx, rpc.JoinRequest{NodeID: "0000000000000002", Name: "two", Key: "not base64!", RaftPort: 1, RPCPort: 2}); !errors.Is(err, ErrBadMemberOp) {
		t.Fatalf("join with a bad key: %v", err)
	}
	resp, err := n.Join(ctx, rpc.JoinRequest{NodeID: "0000000000000002", Name: "two", Key: keyText(k2), MeshPort: 20483, SeenFrom: "203.0.113.4:40000", RaftPort: 1, RPCPort: 2})
	if err != nil {
		t.Fatal(err)
	}
	row, ok := n.FSM.Node(resp.NodeIdx)
	if !ok || row.Addr != "100.70.3.2" || row.Key != k2 || row.MeshPort != 20483 || len(row.Endpoints) != 1 || row.Endpoints[0] != "203.0.113.4:40000" {
		t.Fatalf("joined row %+v", row)
	}
	if len(n.FSM.Pending()) != 0 {
		t.Fatal("the join left its pending row")
	}
	if byKey, ok := n.FSM.NodeByKey(k2); !ok || byKey.Idx != row.Idx {
		t.Fatalf("NodeByKey %+v %v", byKey, ok)
	}
	// the members the response lists carry their keys, for the joiner's seed and peers
	if len(resp.Members) != 2 || resp.Members[0].Key != meshKey(1) || resp.Members[1].Key != k2 {
		t.Fatalf("members %+v", resp.Members)
	}
	// a member's key introduced again gets its own address, no pending row
	if own, err := n.Introduce(ctx, k2); err != nil || own != addr || len(n.FSM.Pending()) != 0 {
		t.Fatalf("a member's introduce: %v %v", own, err)
	}
	// its candidates
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "update_endpoints", NodeIdx: row.Idx, Endpoints: []string{"192.168.1.9:20483", "203.0.113.4:40000"}}); err != nil {
		t.Fatal(err)
	}
	if row, _ = n.FSM.Node(resp.NodeIdx); len(row.Endpoints) != 2 || row.Endpoints[0] != "192.168.1.9:20483" {
		t.Fatalf("endpoints %+v", row.Endpoints)
	}
	// a device that was reset: a new node ID, the same key, the row's own address
	resp2, err := n.Join(ctx, rpc.JoinRequest{NodeID: "0000000000000022", Name: "two", Key: keyText(k2), MeshPort: 20483, RaftPort: 1, RPCPort: 2})
	if err != nil {
		t.Fatalf("re-join after reset: %v", err)
	}
	if resp2.NodeIdx != resp.NodeIdx {
		t.Fatalf("the re-join took index %d, want the row's %d", resp2.NodeIdx, resp.NodeIdx)
	}
	if row, _ = n.FSM.Node(resp.NodeIdx); row.ID != "0000000000000022" || row.Addr != "100.70.3.2" || row.Key != k2 {
		t.Fatalf("re-keyed row %+v", row)
	}
	servers := n.Raft.GetConfiguration().Configuration().Servers
	seen := map[raft.ServerID]int{}
	for _, s := range servers {
		seen[s.ID]++
	}
	if seen["0000000000000002"] != 0 || seen["0000000000000022"] != 1 {
		t.Fatalf("configuration %+v", servers)
	}
	// another key cannot take the node ID
	if _, err := n.Join(ctx, rpc.JoinRequest{NodeID: "0000000000000022", Name: "two", Key: keyText(k3), RaftPort: 1, RPCPort: 2}); err == nil {
		t.Fatal("another key took a node ID")
	}
	// expiry: a fresh pending row stays, an old one goes
	if _, err := n.Introduce(ctx, k3); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if removed, err := n.ExpirePending(ctx, now); err != nil || removed != 0 || len(n.FSM.Pending()) != 1 {
		t.Fatalf("expiry of a fresh row: %d %v", removed, err)
	}
	if removed, err := n.ExpirePending(ctx, now.Add(meta.PendingTTL+time.Second)); err != nil || removed != 1 || len(n.FSM.Pending()) != 0 {
		t.Fatalf("expiry: %d %v", removed, err)
	}
	if _, err := n.Join(ctx, rpc.JoinRequest{NodeID: "0000000000000003", Name: "three", Key: keyText(k3), RaftPort: 1, RPCPort: 2}); !errors.Is(err, ErrNoIntroduce) {
		t.Fatalf("join after the expiry: %v", err)
	}
}
