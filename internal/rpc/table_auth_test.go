package rpc

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// Invariant: I-318
func TestTableAuthorizer_Table(t *testing.T) {
	member := meta.Node{Idx: 2, ID: "m", Kind: meta.KindDevice, State: meta.Active, Addr: "100.70.3.2"}
	removed := meta.Node{Idx: 3, ID: "r", Kind: meta.KindDevice, State: meta.Removed, Addr: "100.70.3.3"}
	seeded := meta.Node{Idx: 4, ID: "s", Kind: meta.KindDevice, State: meta.Active, Addr: "100.70.3.4"}
	a := &TableAuthorizer{}
	a.SetMembers(func(addr string) (meta.Node, bool) {
		switch addr {
		case member.Addr:
			return member, true
		case removed.Addr:
			return removed, true
		}
		return meta.Node{}, false
	})
	a.SetPending(func(addr string) bool { return addr == "100.70.3.9" })
	a.SetSeed([]meta.Node{seeded, {Idx: 5, Kind: meta.KindDisk, Addr: "100.70.3.5"}, {Idx: 6, Kind: meta.KindDevice, State: meta.Removed, Addr: "100.70.3.6"}})
	cases := []struct {
		src     string
		allowed bool
		pending bool
		idx     meta.NodeIdx
	}{
		{"100.70.3.2", true, false, 2},
		{"::ffff:100.70.3.2", true, false, 2}, // a mapped source is the same address
		{"100.70.3.3", false, false, 0},       // removed
		{"100.70.3.4", true, false, 4},        // the seed, until the table has it
		{"100.70.3.5", false, false, 0},       // a disk row never seeds
		{"100.70.3.6", false, false, 0},       // a removed row never seeds
		{"100.70.3.9", false, true, 0},        // pending: /health and /v1/join only
		{"100.70.3.77", false, false, 0},
		{"203.0.113.9", false, false, 0},
	}
	for _, tc := range cases {
		src := netip.MustParseAddr(tc.src)
		m, err := a.Allow(context.Background(), src)
		if (err == nil) != tc.allowed || (err != nil && !errors.Is(err, ErrForbidden)) || m.Node.Idx != tc.idx {
			t.Errorf("%s: allowed=%v idx=%d err=%v", tc.src, err == nil, m.Node.Idx, err)
		}
		if got := a.IsPending(src); got != tc.pending {
			t.Errorf("%s: pending=%v, want %v", tc.src, got, tc.pending)
		}
	}
	changes := 0
	a.OnChange = func() { changes++ }
	a.Purge() // nothing to drop: the hook runs
	if changes != 1 {
		t.Fatalf("Purge ran the hook %d times", changes)
	}
	if s := a.Seed(); len(s) != 1 || s[0].Idx != 4 {
		t.Fatalf("Seed: %+v", s)
	}
	var none TableAuthorizer
	if _, err := none.Allow(context.Background(), netip.MustParseAddr("100.70.3.2")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an unbound authorizer admits: %v", err)
	}
	if none.IsPending(netip.MustParseAddr("100.70.3.9")) {
		t.Fatal("an unbound authorizer has pending rows")
	}
	var _ Authorizer = &none
}

// A member publishes its own candidates and its own version and nobody else's: the
// unsigned member ops name the caller's row alone; /health tells a caller the endpoint
// the device sees for it.
// Invariant: I-322, I-345
func TestMembers_UpdateEndpointsOwnRowOnly(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	e.app.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "n2", Name: "two", Kind: meta.KindDevice, TSStableID: "someone-else", Addr: "100.64.0.60"}}})
	if err := c.Members(ctx, e.addr, MemberOp{Op: "update_endpoints", NodeIdx: 2, Endpoints: []string{"1.2.3.4:1"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another row's endpoints: %v, want ErrForbidden", err)
	}
	if err := c.Members(ctx, e.addr, MemberOp{Op: "set_version", NodeIdx: 2, Version: "v9"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another row's version: %v, want ErrForbidden", err)
	}
	if len(e.joiner.ops) != 0 {
		t.Fatalf("the refused op reached the joiner: %+v", e.joiner.ops)
	}
	if err := c.Members(ctx, e.addr, MemberOp{Op: "update_endpoints", NodeIdx: 1, Endpoints: []string{"1.2.3.4:1", "[2001:db8::1]:1"}}); err != nil {
		t.Fatalf("own endpoints: %v", err)
	}
	if len(e.joiner.ops) != 1 || e.joiner.ops[0].Op != "update_endpoints" || len(e.joiner.ops[0].Endpoints) != 2 {
		t.Fatalf("op passed to the joiner: %+v", e.joiner.ops)
	}
	h, err := c.Health(ctx, e.addr)
	if err != nil || h.SeenFrom != "" {
		t.Fatalf("health without a device: %+v %v", h, err)
	}
	e.srv.o.SeenFrom = func(src netip.Addr) string { return src.String() + ":20483" }
	h, err = c.Health(ctx, e.addr)
	if err != nil || h.SeenFrom != memberIP.String()+":20483" {
		t.Fatalf("health seen_from %q %v, want the caller's", h.SeenFrom, err)
	}
}
