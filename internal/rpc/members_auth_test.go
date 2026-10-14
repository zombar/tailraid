package rpc

import (
	"errors"
	"testing"
)

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
