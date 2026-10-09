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
// The same object gates the Raft transport's accepted connections (cluster.PeerAuth,
// I-92), so it can exist before the FSM does: SetMembers binds the table once the node is
// open, and SetSeed adds the members a join response named, for the window in which a
// joiner's own table is still empty.
type WhoIsAuthorizer struct {
	TS     tailscale.Client
	Member func(addr string) (meta.Node, bool) // the device record at addr, if any (meta.Store.NodeByAddr)
	Clk    clock.Clock
	PosTTL time.Duration // default 10 min
	NegTTL time.Duration // default 30 s

	mu       sync.Mutex
	seed     map[string]meta.Node // by address; consulted when Member has no record (join)
	cache    map[netip.Addr]cacheEntry
	inflight map[netip.Addr]chan struct{} // one WhoIs per address at a time; concurrent misses wait
}

// SetMembers binds the member table (the node's FSM) after construction.
func (a *WhoIsAuthorizer) SetMembers(fn func(addr string) (meta.Node, bool)) {
	a.mu.Lock()
	a.Member = fn
	a.mu.Unlock()
}

// SetSeed records the device members a join response listed, so the joiner accepts the
// leader's Raft connection before its own table holds anyone. The FSM, once replicated,
// takes precedence; the seed only ever names members the leader vouched for.
func (a *WhoIsAuthorizer) SetSeed(members []meta.Node) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seed = map[string]meta.Node{}
	for _, m := range members {
		if m.Kind == meta.KindDevice && m.State != meta.Removed && m.Addr != "" {
			a.seed[m.Addr] = m
		}
	}
}

// member is the record at addr: the table first, then the join seed.
func (a *WhoIsAuthorizer) member(addr string) (meta.Node, bool) {
	a.mu.Lock()
	lookup := a.Member
	seeded, inSeed := a.seed[addr]
	a.mu.Unlock()
	if lookup != nil {
		if m, ok := lookup(addr); ok {
			return m, true
		}
	}
	return seeded, inSeed
}

type cacheEntry struct {
	stableID string
	login    string // tailnet user, "" for tagged devices (docs/13 §2)
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

// Identify resolves src through WhoIs with positive and negative caching; concurrent
// misses for one address share a single WhoIs call (a burst of shard PUTs from a new
// peer costs one shell-out, not one per request).
func (a *WhoIsAuthorizer) Identify(ctx context.Context, src netip.Addr) (string, error) {
	e, err := a.lookup(ctx, src)
	return e.stableID, err
}

// User resolves src to its StableID and tailnet login through the same cache (docs/13 §2).
func (a *WhoIsAuthorizer) User(ctx context.Context, src netip.Addr) (stableID, login string, err error) {
	e, err := a.lookup(ctx, src)
	return e.stableID, e.login, err
}

// lookup is the cached, single-flight WhoIs behind Identify and User.
func (a *WhoIsAuthorizer) lookup(ctx context.Context, src netip.Addr) (cacheEntry, error) {
	for {
		a.mu.Lock()
		if a.cache == nil {
			a.cache = map[netip.Addr]cacheEntry{}
		}
		if a.inflight == nil {
			a.inflight = map[netip.Addr]chan struct{}{}
		}
		if e, ok := a.cache[src]; ok && a.now().Before(e.expires) {
			a.mu.Unlock()
			return e, e.err
		}
		wait, running := a.inflight[src]
		if !running {
			wait = make(chan struct{})
			a.inflight[src] = wait
		}
		a.mu.Unlock()
		if running {
			select {
			case <-wait:
				continue // the result is cached now, or the call failed and ours may fare better
			case <-ctx.Done():
				return cacheEntry{}, ctx.Err()
			}
		}
		e, err := a.whois(ctx, src)
		a.mu.Lock()
		delete(a.inflight, src)
		a.mu.Unlock()
		close(wait)
		return e, err
	}
}

// whois is the uncached lookup; it caches positive and not-found answers.
func (a *WhoIsAuthorizer) whois(ctx context.Context, src netip.Addr) (cacheEntry, error) {
	pos, neg := a.ttls()
	w, err := a.TS.WhoIs(ctx, src)
	e := cacheEntry{}
	if err != nil {
		if errors.Is(err, tailscale.ErrNotFound) {
			e.err = fmt.Errorf("%w: %s is not on the tailnet", ErrForbidden, src)
			e.expires = a.now().Add(neg)
		} else {
			return cacheEntry{}, fmt.Errorf("%w: whois: %v", ErrUnavailable, err) //nolint:errorlint // transport failure, not a sentinel to unwrap
		}
	} else {
		e.stableID, e.login, e.expires = w.Node.StableID, w.Login(), a.now().Add(pos)
	}
	a.mu.Lock()
	a.cache[src] = e
	a.mu.Unlock()
	return e, e.err
}

// Allow implements docs/08 steps 2–4: member by IP, then WhoIs StableID must match.
func (a *WhoIsAuthorizer) Allow(ctx context.Context, src netip.Addr) (meta.Node, error) {
	member, found := a.member(src.String())
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

// AllowAll answers every source with the given member and StableID. It is a test fake
// (the harness and the webui, dev and rpc tests); nothing in production uses it, and
// Listen refuses a non-loopback bind without a real authorizer.
type AllowAll struct{ Node meta.Node }

func (AllowAll) Purge() {}

func (a AllowAll) Allow(context.Context, netip.Addr) (meta.Node, error) { return a.Node, nil }

func (a AllowAll) Identify(context.Context, netip.Addr) (string, error) {
	return a.Node.TSStableID, nil
}

func (a AllowAll) User(context.Context, netip.Addr) (string, string, error) {
	return a.Node.TSStableID, "", nil
}
