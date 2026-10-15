package node

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/zombar/tailraid/internal/tailscale"
)

// join resolves a device name through the tailnet status (host name, MagicDNS name,
// first label), keeps a tailnet IP with or without a port, and refuses anything off the
// tailnet, where no node listens.
//
// Invariant: I-76
func TestResolveJoinTarget_Table(t *testing.T) {
	st := &tailscale.Status{
		MagicDNSSuffix: "tail-abc.ts.net",
		TailscaleIPs:   []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		Self:           tailscale.PeerStatus{HostName: "laptop", DNSName: "laptop.tail-abc.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.12")}},
		Peer: map[string]tailscale.PeerStatus{
			"k1": {HostName: "jockey", DNSName: "jockey.tail-abc.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::1"), netip.MustParseAddr("100.64.0.11")}},
			"k2": {HostName: "Nas", DNSName: "nas.tail-abc.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.13")}},
			"k3": {HostName: "v6only", DNSName: "v6only.tail-abc.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::7")}},
		},
	}
	for _, c := range []struct {
		host string
		want string
		err  error
	}{
		{"100.64.0.11", "100.64.0.11:20481", nil},
		{"100.64.0.11:20499", "100.64.0.11:20499", nil},
		{"fd7a:115c:a1e0::1", "[fd7a:115c:a1e0::1]:20481", nil},
		{"[fd7a:115c:a1e0::1]:20499", "[fd7a:115c:a1e0::1]:20499", nil},
		{"jockey", "100.64.0.11:20481", nil}, // the IPv4 of a dual-stack peer
		{"JOCKEY", "100.64.0.11:20481", nil},
		{"jockey.tail-abc.ts.net", "100.64.0.11:20481", nil},
		{"jockey.tail-abc.ts.net.", "100.64.0.11:20481", nil},
		{"jockey:20499", "100.64.0.11:20499", nil},
		{"nas", "100.64.0.13:20481", nil},
		{"v6only", "[fd7a:115c:a1e0::7]:20481", nil},
		{"laptop", "100.64.0.12:20481", nil}, // self resolves too (joining one's own cluster is refused later)
		{"192.168.1.126", "", ErrNotTailnet},
		{"127.0.0.1", "127.0.0.1:20481", nil}, // listed by the status (a dev fake on loopback)
		{"192.168.1.126:20481", "", ErrNotTailnet},
		{"jockey.local", "", ErrNotTailnet},
		{"nowhere", "", ErrNotTailnet},
		{"", "", ErrNotTailnet},
	} {
		got, err := ResolveJoinTarget(st, c.host, 20481)
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("%q: %q %v, want %q %v", c.host, got, err, c.want, c.err)
		}
	}
	// no status at all (tailscaled unreachable): an IP still works, a name does not
	if got, err := ResolveJoinTarget(nil, "100.64.0.11", 1); err != nil || got != "100.64.0.11:1" {
		t.Fatalf("no status, IP: %q %v", got, err)
	}
	if _, err := ResolveJoinTarget(nil, "jockey", 1); !errors.Is(err, ErrNotTailnet) {
		t.Fatalf("no status, name: %v", err)
	}
}
