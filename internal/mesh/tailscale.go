package mesh

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

// The mesh never runs inside a tailnet (docs/20 §5, I-336): Tailscale's space is
// 100.64.0.0/10, where the mesh's own subnets also live, and its IPv6 ULA.
var tailscaleSpace = []netip.Prefix{meshSpace, netip.MustParsePrefix("fd7a:115c:a1e0::/48")}

// RouteTTL is how long the bind trusts the kernel's answer for one destination.
const RouteTTL = 30 * time.Second

// routeCacheMax bounds the cache; past it the cache starts over.
const routeCacheMax = 1024

var errViaTailscale = errors.New("mesh: the destination is reached through Tailscale: not a mesh path")

// inTailscale reports an address in Tailscale's space.
func inTailscale(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range tailscaleSpace {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// usableCandidate reports an interface address a peer may be given as a candidate
// (I-322, I-336).
func usableCandidate(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsMulticast() && !ip.IsUnspecified() && !inTailscale(ip)
}

// routeCache answers whether the host reaches a destination through Tailscale: the
// destination is in its space, or the kernel would send to it from an address in it (a
// subnet route, an exit node).
type routeCache struct {
	src func(dst netip.Addr) (netip.Addr, error) // nil: kernelSource
	now func() time.Time                         // nil: time.Now

	mu   sync.Mutex
	seen map[netip.Addr]routeEntry
}

type routeEntry struct {
	via bool
	at  time.Time
}

func (c *routeCache) viaTailscale(dst netip.Addr) bool {
	dst = dst.Unmap()
	if inTailscale(dst) {
		return true
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	t := now()
	c.mu.Lock()
	e, ok := c.seen[dst]
	c.mu.Unlock()
	if ok && t.Sub(e.at) < RouteTTL {
		return e.via
	}
	src := c.src
	if src == nil {
		src = kernelSource
	}
	s, err := src(dst)
	via := err == nil && inTailscale(s) // no route: the send fails on its own
	c.mu.Lock()
	if c.seen == nil || len(c.seen) >= routeCacheMax {
		c.seen = map[netip.Addr]routeEntry{}
	}
	c.seen[dst] = routeEntry{via: via, at: t}
	c.mu.Unlock()
	return via
}

// kernelSource is the source address the kernel picks for dst: a connected UDP socket
// sends nothing and reports it.
func kernelSource(dst netip.Addr) (netip.Addr, error) {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(dst, 9)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, errors.New("mesh: no local address")
	}
	return ua.AddrPort().Addr().Unmap(), nil
}
