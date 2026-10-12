package cluster

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// A mirror row is keyed by its marker's ID: a retry updates it and keeps its name, and
// it resolves to its host's address, where its objects and trees are served (docs/16 §1,
// §6).
//
// Invariant: I-185, I-188
func TestMember_AddMirror_RetryKeepsRowAndResolvesToHost(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	self, _ := n.SelfIdx()
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "update_addr", NodeIdx: self, Addr: "100.64.0.9"}); err != nil {
		t.Fatal(err)
	}
	op := rpc.MemberOp{Op: "add_mirror", NodeIdx: self, DiskID: "feed0123feed0123", Name: "a/m1"}
	for range 2 {
		if err := n.MemberOp(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "add_mirror", NodeIdx: self, Name: "a/m2"}); !errors.Is(err, ErrBadMemberOp) {
		t.Fatalf("no mirror ID: %v", err)
	}
	var rows []meta.Node
	for _, nd := range n.FSM.Nodes() {
		if nd.Kind == meta.KindMirror {
			rows = append(rows, nd)
		}
	}
	if len(rows) != 1 || rows[0].Host != self || rows[0].Name != "a/m1" || rows[0].Kind.Target() {
		t.Fatalf("mirror rows %+v", rows)
	}
	op.Name = "renamed"
	if err := n.MemberOp(ctx, op); err != nil {
		t.Fatal(err)
	}
	if nd, _ := n.FSM.Node(rows[0].Idx); nd.Name != "a/m1" {
		t.Fatalf("a re-add renamed the row: %+v", nd)
	}
	hostAddr, _, _ := n.RPCAddrOf(self)
	if addr, isSelf, ok := n.RPCAddrOf(rows[0].Idx); !ok || !isSelf || addr != hostAddr {
		t.Fatalf("RPCAddrOf(mirror) = %q %v %v, want %q", addr, isSelf, ok, hostAddr)
	}
	if _, ok := n.DiskOf(rows[0].Idx); ok {
		t.Fatal("a mirror is not a disk")
	}
}
