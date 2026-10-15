package rpc

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// update_addr arrives from an address the member table does not know (docs/08 step 6):
// it is allowed when WhoIs(src) is the StableID of the member it names, it may change
// nothing but that member's address, and every other op from a non-member is refused.
//
// Invariant: I-93
func TestMembers_UpdateAddrFromUnknownAddress(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	// the member record moves elsewhere: the caller's address is no longer in the table
	e.app.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "n1", Name: "one", Kind: meta.KindDevice, TSStableID: "self-1", Addr: "100.64.0.50", Capacity: 1 << 30}}})
	e.app.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "n2", Name: "two", Kind: meta.KindDevice, TSStableID: "someone-else", Addr: "100.64.0.60", Capacity: 1 << 30}}})
	e.auth.Purge()
	if err := c.Members(ctx, e.addr, MemberOp{Op: "set_capacity", NodeIdx: 1, Capacity: 7}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("set_capacity from a non-member address: %v, want ErrForbidden", err)
	}
	if err := c.Members(ctx, e.addr, MemberOp{Op: "update_addr", NodeIdx: 2, Addr: memberIP.String()}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("update_addr for another device's record: %v, want ErrForbidden", err)
	}
	if len(e.joiner.ops) != 0 {
		t.Fatalf("refused ops reached the joiner: %+v", e.joiner.ops)
	}
	// the device itself, from its new address: allowed, and only the address travels
	if err := c.Members(ctx, e.addr, MemberOp{Op: "update_addr", NodeIdx: 1, Addr: memberIP.String(), Capacity: 9, Force: true}); err != nil {
		t.Fatalf("update_addr by the device: %v", err)
	}
	if len(e.joiner.ops) != 1 || e.joiner.ops[0].Op != "update_addr" || e.joiner.ops[0].NodeIdx != 1 || e.joiner.ops[0].Addr != memberIP.String() || e.joiner.ops[0].Capacity != 0 || e.joiner.ops[0].Force {
		t.Fatalf("op passed to the joiner: %+v", e.joiner.ops)
	}
}

// The address a join records is the caller's own (docs/08 §Join): a token holder cannot
// point a member record at another device.
//
// Invariant: I-94
func TestJoin_AddrMustBeCaller(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	req := JoinRequest{Token: "tok", NodeID: "n2", Name: "two", TSStableID: "self-1", Addr: "100.64.0.77", RaftPort: 1, RPCPort: 2}
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("join with another address: %v, want ErrForbidden", err)
	}
	if len(e.app.fsm.Nodes()) != 1 {
		t.Fatal("a refused join changed membership")
	}
	req.Addr = ""
	if _, err := c.Join(ctx, e.addr, req); err != nil {
		t.Fatalf("join without an address: %v", err)
	}
	if n := e.app.fsm.Nodes()[1]; n.Addr != memberIP.String() {
		t.Fatalf("recorded address %q, want the caller's %s", n.Addr, memberIP)
	}
}
