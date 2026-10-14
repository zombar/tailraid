package rpc

import (
	"context"
	"fmt"
	"net/netip"
	"sync"

	"github.com/zombar/tailraid/internal/meta"
)

// Principal is who a request comes from: today the member row at its source address.
// A user with groups is the planned next layer (docs/20 §11, after 1.0): a client row
// would carry an owner, and a permission check would run in the FSM's apply path as the
// protected flag does, so the type is here now and the rows are not reshaped later.
type Principal struct {
	Node meta.Node
}

// Authorizer decides whether a source address may call member endpoints (docs/08 §Node
// RPC authorization).
type Authorizer interface {
	// Allow returns the principal behind src, or ErrForbidden.
	Allow(ctx context.Context, src netip.Addr) (Principal, error)
	// Purge tells the authorizer the membership changed.
	Purge()
}

// PendingAuthorizer is an Authorizer that also knows the joins in progress (docs/20 §4):
// a pending source may call /health and POST /v1/join and nothing else.
type PendingAuthorizer interface {
	Authorizer
	IsPending(src netip.Addr) bool
}

// AllowAll authorizes every caller as Node (tests only).
type AllowAll struct{ Node meta.Node }

func (AllowAll) Purge() {}

func (a AllowAll) Allow(context.Context, netip.Addr) (Principal, error) {
	return Principal(a), nil
}

// TableAuthorizer authorizes by the member table alone (docs/08 §Node RPC authorization,
// docs/20 §3, I-318): on the mesh a packet from an address was sealed by the key that
// owns it, so the row at the source address is the whole check. A pending row (a join
// in progress) is admitted to /health and /v1/join only. It exists before the FSM does:
// SetMembers binds the table, SetSeed the members a join response named for the window
// in which a joiner's own table is still empty.
type TableAuthorizer struct {
	Member  func(addr string) (meta.Node, bool) // the active device row at addr (meta.FSM.NodeByAddr)
	Pending func(addr string) bool              // a pending row at addr (meta.FSM.PendingByAddr)
	// OnChange, when set, runs on every Purge: the node refreshes its peers from the table.
	OnChange func()

	mu   sync.Mutex
	seed map[string]meta.Node
}

var _ PendingAuthorizer = (*TableAuthorizer)(nil)

// SetMembers binds the member table after construction.
func (a *TableAuthorizer) SetMembers(fn func(addr string) (meta.Node, bool)) {
	a.mu.Lock()
	a.Member = fn
	a.mu.Unlock()
}

// SetPending binds the pending rows after construction.
func (a *TableAuthorizer) SetPending(fn func(addr string) bool) {
	a.mu.Lock()
	a.Pending = fn
	a.mu.Unlock()
}

// SetSeed records the device members a join response listed (docs/08 §Join).
func (a *TableAuthorizer) SetSeed(members []meta.Node) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seed = seedOf(members)
}

// Seed is the seed as last set, for the peer table that follows it (docs/20 §3).
func (a *TableAuthorizer) Seed() []meta.Node {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]meta.Node, 0, len(a.seed))
	for k := range a.seed {
		out = append(out, a.seed[k])
	}
	return out
}

// seedOf is the join seed: the device rows with an address that are not removed.
func seedOf(members []meta.Node) map[string]meta.Node {
	seed := map[string]meta.Node{}
	for i := range members {
		if m := &members[i]; m.Kind == meta.KindDevice && m.State != meta.Removed && m.Addr != "" {
			seed[m.Addr] = *m
		}
	}
	return seed
}

// Allow returns the active member at src: the table first, then the seed.
func (a *TableAuthorizer) Allow(_ context.Context, src netip.Addr) (Principal, error) {
	addr := src.Unmap().String()
	a.mu.Lock()
	lookup := a.Member
	seeded, inSeed := a.seed[addr]
	a.mu.Unlock()
	if lookup != nil {
		if m, ok := lookup(addr); ok && m.State != meta.Removed {
			return Principal{Node: m}, nil
		}
	}
	if inSeed {
		return Principal{Node: seeded}, nil
	}
	return Principal{}, fmt.Errorf("%w: %s is not a member", ErrForbidden, addr)
}

// IsPending reports whether a join is in progress at src (docs/20 §4).
func (a *TableAuthorizer) IsPending(src netip.Addr) bool {
	a.mu.Lock()
	pending := a.Pending
	a.mu.Unlock()
	return pending != nil && pending(src.Unmap().String())
}

// Purge has no cache to drop (I-318); it runs OnChange, the node's peer refresh.
func (a *TableAuthorizer) Purge() {
	a.mu.Lock()
	hook := a.OnChange
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
}
