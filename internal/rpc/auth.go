package rpc

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/tailscale"
)

// Authorizer decides whether a source address may call member endpoints (docs/08 §auth).
type Authorizer interface {
	// Allow returns the member behind src, or ErrForbidden.
	Allow(ctx context.Context, src netip.Addr) (meta.Node, error)
	// Identify returns the tailnet StableID behind src (join: no membership needed).
	Identify(ctx context.Context, src netip.Addr) (string, error)
	// Purge drops cached identities (membership changed).
	Purge()
}

// WhoIsAuthorizer authorizes by member table then WhoIs StableID (docs/08 steps 1–4).
type WhoIsAuthorizer struct {
	TS      tailscale.Client
	Members func() []meta.Node
	Clk     clock.Clock
	PosTTL  time.Duration // default 10 min
	NegTTL  time.Duration // default 30 s

	mu    sync.Mutex
	cache map[netip.Addr]cacheEntry
}

type cacheEntry struct {
	stableID string
	err      error
	expires  time.Time
}

var _ Authorizer = (*WhoIsAuthorizer)(nil)

func (a *WhoIsAuthorizer) ttls() (time.Duration, time.Duration) {
	pos, neg := a.PosTTL, a.NegTTL
	if pos == 0 {
		pos = 10 * time.Minute
	}
	if neg == 0 {
		neg = 30 * time.Second
	}
	return pos, neg
}

func (a *WhoIsAuthorizer) now() time.Time {
	if a.Clk != nil {
		return a.Clk.Now()
	}
	return time.Now()
}

// Identify resolves src through WhoIs with positive and negative caching.
func (a *WhoIsAuthorizer) Identify(ctx context.Context, src netip.Addr) (string, error) {
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[netip.Addr]cacheEntry{}
	}
	if e, ok := a.cache[src]; ok && a.now().Before(e.expires) {
		a.mu.Unlock()
		return e.stableID, e.err
	}
	a.mu.Unlock()
	pos, neg := a.ttls()
	w, err := a.TS.WhoIs(ctx, src)
	e := cacheEntry{}
	if err != nil {
		if errors.Is(err, tailscale.ErrNotFound) {
			e.err = fmt.Errorf("%w: %s is not on the tailnet", ErrForbidden, src)
			e.expires = a.now().Add(neg)
		} else {
			return "", fmt.Errorf("%w: whois: %v", ErrUnavailable, err) //nolint:errorlint // transport failure, not a sentinel to unwrap
		}
	} else {
		e.stableID, e.expires = w.Node.StableID, a.now().Add(pos)
	}
	a.mu.Lock()
	a.cache[src] = e
	a.mu.Unlock()
	return e.stableID, e.err
}

// Allow implements docs/08 steps 2–4: member by IP, then WhoIs StableID must match.
func (a *WhoIsAuthorizer) Allow(ctx context.Context, src netip.Addr) (meta.Node, error) {
	var member meta.Node
	found := false
	nodes := a.Members()
	for i := range nodes {
		if nodes[i].Kind == meta.KindDevice && nodes[i].State != meta.Removed && nodes[i].Addr == src.String() {
			member, found = nodes[i], true
			break
		}
	}
	if !found {
		return meta.Node{}, fmt.Errorf("%w: %s is not a member", ErrForbidden, src)
	}
	id, err := a.Identify(ctx, src)
	if err != nil {
		return meta.Node{}, err
	}
	if id != member.TSStableID {
		a.mu.Lock()
		delete(a.cache, src)
		a.mu.Unlock()
		return meta.Node{}, fmt.Errorf("%w: %s belongs to tailnet node %s, member record says %s", ErrForbidden, src, id, member.TSStableID)
	}
	return member, nil
}

// Purge clears the identity cache.
func (a *WhoIsAuthorizer) Purge() {
	a.mu.Lock()
	a.cache = map[netip.Addr]cacheEntry{}
	a.mu.Unlock()
}

// AllowAll is the loopback/dev authorizer: every source is member idx 1 with the given
// StableID. Only used by tests and single-node serve.
type AllowAll struct{ Node meta.Node }

func (AllowAll) Purge() {}

func (a AllowAll) Allow(context.Context, netip.Addr) (meta.Node, error) { return a.Node, nil }

func (a AllowAll) Identify(context.Context, netip.Addr) (string, error) {
	return a.Node.TSStableID, nil
}
