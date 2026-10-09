package node

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/tailscale"
)

// Tailscale's address ranges: every node IP is in the CGNAT /10 (IPv4) or the ULA /48.
var (
	tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// ErrNotTailnet is a join target that is reachable only off the tailnet, where no node
// listens (docs/08, I-50).
var ErrNotTailnet = errors.New("node: not a Tailscale address")

// ResolveJoinTarget turns what the operator typed into the host:port of a node's RPC
// (docs/12 §7): a tailnet IP (Tailscale's ranges, or any address the tailnet status
// lists, which is how a dev fake on loopback works) with or without a port is used as
// is; a device name is looked up in the tailnet status (host name, MagicDNS name with or
// without the suffix) and its Tailscale IP taken, since a plain DNS lookup would give a
// LAN address the node does not listen on; any other address is refused with
// ErrNotTailnet. Without a status (tailscaled unreachable) an IP is taken as typed. port
// is the RPC port to add when none was given.
func ResolveJoinTarget(st *tailscale.Status, host string, port int) (string, error) {
	h, p := host, strconv.Itoa(port)
	if hh, pp, err := net.SplitHostPort(host); err == nil {
		h, p = hh, pp
	}
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		if st != nil && !tailnetV4.Contains(ip) && !tailnetV6.Contains(ip) && !listedIP(st, ip) {
			return "", fmt.Errorf("%w: %s; a node listens only on its Tailscale IP (tailscale status shows it)", ErrNotTailnet, ip)
		}
		return net.JoinHostPort(ip.String(), p), nil
	}
	want := strings.ToLower(strings.TrimSuffix(h, "."))
	if st != nil {
		peers := append([]*tailscale.PeerStatus{&st.Self}, peersOf(st)...)
		for _, peer := range peers {
			if matchesPeer(peer, want) {
				for _, ip := range peer.TailscaleIPs {
					if ip.Is4() {
						return net.JoinHostPort(ip.String(), p), nil
					}
				}
				if len(peer.TailscaleIPs) > 0 {
					return net.JoinHostPort(peer.TailscaleIPs[0].String(), p), nil
				}
			}
		}
	}
	var names []string
	if st != nil {
		for _, peer := range peersOf(st) {
			names = append(names, peer.HostName)
		}
	}
	if len(names) > 0 {
		return "", fmt.Errorf("%w: no tailnet device is named %q (known: %s); use the device's Tailscale IP or its MagicDNS name", ErrNotTailnet, host, strings.Join(names, ", "))
	}
	return "", fmt.Errorf("%w: no tailnet device is named %q; use the device's Tailscale IP", ErrNotTailnet, host)
}

func peersOf(st *tailscale.Status) []*tailscale.PeerStatus {
	out := make([]*tailscale.PeerStatus, 0, len(st.Peer))
	for k := range st.Peer {
		p := st.Peer[k]
		out = append(out, &p)
	}
	return out
}

// listedIP reports whether the tailnet status names ip as one of its own addresses or a
// peer's.
func listedIP(st *tailscale.Status, ip netip.Addr) bool {
	if slices.Contains(st.TailscaleIPs, ip) {
		return true
	}
	for _, peer := range append([]*tailscale.PeerStatus{&st.Self}, peersOf(st)...) {
		if slices.Contains(peer.TailscaleIPs, ip) {
			return true
		}
	}
	return false
}

// matchesPeer: the host name, the MagicDNS name, or its first label, case-insensitively.
func matchesPeer(p *tailscale.PeerStatus, want string) bool {
	dns := strings.ToLower(strings.TrimSuffix(p.DNSName, "."))
	first, _, _ := strings.Cut(dns, ".")
	return want != "" && (strings.EqualFold(p.HostName, want) || dns == want || first == want)
}
