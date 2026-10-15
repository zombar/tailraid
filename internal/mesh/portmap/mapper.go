package portmap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// The mapper's rhythm (docs/20 §7, I-330).
const (
	// MapLifetime is what the mapper asks for; a gateway may grant less.
	MapLifetime = time.Hour
	// RetryEvery is how often a mapper with no gateway, or a refused mapping, tries again.
	RetryEvery = time.Minute
	// CheckEvery is how often a held mapping's gateway is probed for its epoch: an epoch
	// that went backwards is a router that lost its mappings, and the mapper maps again.
	CheckEvery = time.Minute
	// GatewayPort is the protocol port asked; 0 is Port.
	callTimeout = 3 * time.Second
)

// Mapper holds one mapping of the mesh port for as long as it runs: it finds the gateway,
// maps, refreshes at half the lifetime, maps again when the gateway lost the mapping, and
// releases it at Close. Tick drives it; Status and External report it.
type Mapper struct {
	Port    uint16
	Clock   clock.Clock
	Log     *slog.Logger
	Gateway func(ctx context.Context) (gw, local netip.Addr, err error) // nil: DefaultGateway
	// GatewayPort is the gateway's protocol port (0: Port); the tests point it at a
	// responder on the loopback.
	GatewayPort uint16

	mu      sync.Mutex
	client  Client
	mapping Mapping
	held    bool
	since   time.Time // when the mapping was made
	expires time.Time
	nextTry time.Time // the earliest next probe while nothing is held, or the next check
	why     string    // the last reason nothing is held
}

// Status is the mapper as status reports it (docs/20 §10).
type Status struct {
	Protocol string `json:"protocol"`           // pcp, natpmp or none
	External string `json:"external,omitempty"` // the mapped endpoint
	Gateway  string `json:"gateway,omitempty"`
	Since    string `json:"since,omitempty"` // how long the mapping has been held
	Expires  string `json:"expires,omitempty"`
	Why      string `json:"why,omitempty"` // with none: the last reason
}

func (m *Mapper) clk() clock.Clock {
	if m.Clock == nil {
		return clock.Real()
	}
	return m.Clock
}

func (m *Mapper) log() *slog.Logger {
	if m.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Log
}

// Usable refuses an external endpoint no peer on the internet reaches: an unspecified,
// loopback, private, link-local or multicast address, one inside the mesh's own space
// (100.64/10), or port 0 (I-330). A gateway that answers one is behind another NAT or
// misconfigured; the mapping is given back and tried again later.
func Usable(ip netip.Addr, port uint16) error {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid() || ip.IsUnspecified():
		return errors.New("portmap: the gateway answered no external address")
	case ip.IsLoopback(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsMulticast(), ip.IsPrivate(), cgnat.Contains(ip):
		return fmt.Errorf("portmap: the gateway's external address %s is not reachable from the internet (a second NAT in front of it?)", ip)
	case port == 0:
		return errors.New("portmap: the gateway mapped port 0")
	}
	return nil
}

// cgnat is RFC 6598's shared address space, where the mesh's subnets live (docs/20 §3).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// External is the mapped endpoint while one is held.
func (m *Mapper) External() (netip.AddrPort, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.held {
		return netip.AddrPort{}, false
	}
	return m.mapping.Endpoint(), true
}

// Status reports the mapper.
func (m *Mapper) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.held {
		return Status{Protocol: "none", Why: m.why}
	}
	now := m.clk().Now()
	return Status{
		Protocol: m.mapping.Protocol, External: m.mapping.Endpoint().String(), Gateway: m.mapping.Gateway.String(),
		Since: now.Sub(m.since).Round(time.Second).String(), Expires: m.expires.Sub(now).Round(time.Second).String(),
	}
}

// Tick does what is due: a probe while nothing is held (every RetryEvery), a refresh
// at half the lifetime, an epoch check every CheckEvery.
func (m *Mapper) Tick(ctx context.Context) {
	now := m.clk().Now()
	m.mu.Lock()
	held, nextTry, expires, since := m.held, m.nextTry, m.expires, m.since
	m.mu.Unlock()
	switch {
	case !held && !now.Before(nextTry):
		m.acquire(ctx, now)
	case held && !now.Before(since.Add(expires.Sub(since)/2)):
		m.refresh(ctx, now)
	case held && !now.Before(nextTry):
		m.check(ctx, now)
	}
}

// acquire finds the gateway and asks it for the mapping.
func (m *Mapper) acquire(ctx context.Context, now time.Time) {
	fail := func(why string) {
		m.mu.Lock()
		m.why, m.nextTry = why, now.Add(RetryEvery)
		m.mu.Unlock()
	}
	gwFn := m.Gateway
	if gwFn == nil {
		gwFn = DefaultGateway
	}
	cctx, cancel := context.WithTimeout(ctx, 2*ProbeTimeout+callTimeout)
	defer cancel()
	gw, local, err := gwFn(cctx)
	if err != nil {
		fail(err.Error())
		return
	}
	c, _, _, err := Discover(cctx, gw, local, m.GatewayPort)
	if err != nil {
		fail(err.Error())
		return
	}
	mp, err := c.Map(cctx, m.Port, m.Port, MapLifetime)
	if err != nil {
		fail(err.Error())
		return
	}
	if err := Usable(mp.ExternalIP, mp.External); err != nil {
		// the gateway answered an address no peer reaches: published, it would only
		// draw handshakes into the void (I-330)
		_ = c.Release(cctx, mp)
		fail(err.Error())
		return
	}
	m.mu.Lock()
	m.client, m.mapping, m.held, m.since, m.expires, m.nextTry, m.why = c, mp, true, now, now.Add(mp.Lifetime), now.Add(CheckEvery), ""
	m.mu.Unlock()
	m.log().Info("portmap: mapped", "protocol", mp.Protocol, "gateway", mp.Gateway, "external", mp.Endpoint(), "lifetime", mp.Lifetime)
}

// refresh asks for the same mapping again at half its lifetime; a refusal drops it and
// the next tick starts over.
func (m *Mapper) refresh(ctx context.Context, now time.Time) {
	m.mu.Lock()
	c, mp := m.client, m.mapping
	m.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	again, err := c.Map(cctx, mp.Internal, mp.External, MapLifetime)
	if err != nil {
		m.drop(now, "refresh: "+err.Error())
		return
	}
	if err := Usable(again.ExternalIP, again.External); err != nil {
		m.drop(now, "refresh: "+err.Error())
		return
	}
	m.mu.Lock()
	changed := again.Endpoint() != mp.Endpoint()
	m.mapping, m.since, m.expires, m.nextTry = again, now, now.Add(again.Lifetime), now.Add(CheckEvery)
	m.mu.Unlock()
	if changed {
		m.log().Info("portmap: the refreshed mapping moved", "from", mp.Endpoint(), "to", again.Endpoint())
	}
}

// check probes the gateway's epoch: one older than the mapping's is a router that lost
// its mappings (RFC 6886 §3.6), and the mapping is made again at once.
func (m *Mapper) check(ctx context.Context, now time.Time) {
	m.mu.Lock()
	c, mp, since := m.client, m.mapping, m.since
	m.nextTry = now.Add(CheckEvery)
	m.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	_, epoch, err := c.Probe(cctx)
	if err != nil {
		m.drop(now, "the gateway stopped answering: "+err.Error())
		return
	}
	// the epoch should have advanced by about the time since the mapping; one that
	// lags it by more than a few seconds restarted
	elapsed := uint32(now.Sub(since) / time.Second) //nolint:gosec // seconds
	if epoch+5 < mp.Epoch+elapsed {
		m.log().Info("portmap: the gateway restarted, mapping again", "epoch", epoch, "mapping epoch", mp.Epoch, "held", now.Sub(since))
		m.drop(now, "the gateway restarted")
		m.acquire(ctx, now)
	}
}

// drop forgets the mapping; the next tick starts over at once.
func (m *Mapper) drop(now time.Time, why string) {
	m.mu.Lock()
	m.held, m.client, m.mapping, m.why, m.nextTry = false, nil, Mapping{}, why, now
	m.mu.Unlock()
	m.log().Warn("portmap: mapping lost", "why", why)
}

// Close releases the mapping held, if any.
func (m *Mapper) Close(ctx context.Context) {
	m.mu.Lock()
	c, mp, held := m.client, m.mapping, m.held
	m.held, m.client = false, nil
	m.mu.Unlock()
	if !held || c == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := c.Release(cctx, mp); err != nil {
		m.log().Debug("portmap: release", "err", err)
	}
}
