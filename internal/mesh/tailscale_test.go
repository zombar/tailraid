package mesh

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// No candidate is in Tailscale's space, its IPv6 ULA included; ordinary LAN, public and
// IPv6 addresses are.
// Invariant: I-336
func TestUsableCandidate_Table(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"192.168.1.5", true},
		{"10.19.0.5", true},
		{"104.248.26.224", true},
		{"2a01:4f8::1", true},
		{"fd00::5", true},                 // a home ULA is not Tailscale's
		{"100.64.0.4", false},             // a tailnet address
		{"100.79.112.1", false},           // the mesh's own
		{"100.127.255.254", false},        // the top of 100.64/10
		{"fd7a:115c:a1e0::4", false},      // Tailscale's IPv6
		{"fd7a:115c:a1e0:ab12::1", false}, // anywhere in its /48
		{"127.0.0.1", false},
		{"fe80::1", false},
		{"0.0.0.0", false},
		{"224.0.0.1", false},
	} {
		if got := usableCandidate(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("%s: usable %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// routes is a host's routing table as the bind asks it: the source address the kernel
// would pick for a destination.
type routes map[netip.Addr]netip.Addr

func (r routes) src(dst netip.Addr) (netip.Addr, error) {
	if s, ok := r[dst]; ok {
		return s, nil
	}
	return netip.Addr{}, errors.New("no route")
}

// A fan-out reaches only the candidates the host does not route through Tailscale: not
// one in its space (an older row's), not one behind a subnet route or an exit node.
// Invariant: I-336
func TestBind_NeverSendsOverTailscale(t *testing.T) {
	inner := &recInner{}
	b := newBind(inner, discardLog(), nil)
	lan := netip.MustParseAddr("192.168.1.5")
	routed := netip.MustParseAddr("192.168.50.7") // a LAN behind a Tailscale subnet router
	public := netip.MustParseAddr("203.0.113.5")
	b.routes.src = routes{
		lan:    netip.MustParseAddr("192.168.1.2"),
		routed: netip.MustParseAddr("100.64.0.4"),
		public: netip.MustParseAddr("192.168.1.2"),
	}.src
	cands := []netip.AddrPort{
		netip.AddrPortFrom(netip.MustParseAddr("100.64.0.9"), 20483),
		netip.AddrPortFrom(netip.MustParseAddr("fd7a:115c:a1e0::9"), 20483),
		netip.AddrPortFrom(routed, 20483),
		netip.AddrPortFrom(lan, 20483),
		netip.AddrPortFrom(public, 20483),
	}
	if err := b.SetCandidates(1, cands); err != nil {
		t.Fatal(err)
	}
	if err := b.Send([][]byte{{1, 2, 3}}, &fanout{idx: 1}); err != nil {
		t.Fatal(err)
	}
	var to []string
	for _, s := range inner.sent {
		to = append(to, s.to)
	}
	if len(to) != 2 || to[0] != "192.168.1.5:20483" || to[1] != "203.0.113.5:20483" {
		t.Errorf("sent to %q, want the LAN and the public candidate only", to)
	}

	// a pinned endpoint behind Tailscale and a control frame to one are refused too
	if err := b.Send([][]byte{{1}}, &conn.StdNetEndpoint{AddrPort: netip.AddrPortFrom(routed, 20483)}); !errors.Is(err, errViaTailscale) {
		t.Errorf("pinned send over Tailscale: %v", err)
	}
	if err := b.SendControl([]byte{frameIntroduce}, netip.MustParseAddrPort("[fd7a:115c:a1e0::9]:20483")); !errors.Is(err, errViaTailscale) {
		t.Errorf("control frame to Tailscale: %v", err)
	}
	if err := b.Send([][]byte{{1}}, &relayEP{via: netip.AddrPortFrom(routed, 20483)}); !errors.Is(err, errViaTailscale) {
		t.Errorf("relay through a member behind Tailscale: %v", err)
	}
	if inner.count() != 2 {
		t.Errorf("%d sends, want still 2", inner.count())
	}
}

// A packet arriving from Tailscale's space, WireGuard's or a control frame, never
// reaches WireGuard or a handler, so roaming never pins it; others pass.
// Invariant: I-336
func TestBind_DropsFromTailscale(t *testing.T) {
	b := newBind(&recInner{}, discardLog(), nil)
	from := []string{"100.64.0.9:20483", "[fd7a:115c:a1e0::9]:20483", "192.168.1.5:20483", "100.100.0.1:41641"}
	recv := b.wrap(func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		for i, f := range from {
			packets[i][0] = 4 // a WireGuard transport message
			sizes[i] = 32
			eps[i] = &conn.StdNetEndpoint{AddrPort: netip.MustParseAddrPort(f)}
		}
		return len(from), nil
	})
	packets := make([][]byte, len(from))
	for i := range packets {
		packets[i] = make([]byte, 64)
	}
	sizes := make([]int, len(from))
	eps := make([]conn.Endpoint, len(from))
	n, err := recv(packets, sizes, eps)
	if err != nil || n != len(from) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for i, want := range []int{0, 0, 32, 0} {
		if sizes[i] != want {
			t.Errorf("from %s: size %d, want %d", from[i], sizes[i], want)
		}
	}
}

// The route answer is cached for RouteTTL: a subnet route that appears is honoured once
// the entry expires, and one that goes away frees the destination again.
// Invariant: I-336
func TestBind_TailscaleRouteRecheckedAfterTTL(t *testing.T) {
	inner := &recInner{}
	b := newBind(inner, discardLog(), nil)
	now := time.Unix(1_800_000_000, 0)
	b.routes.now = func() time.Time { return now }
	dst := netip.MustParseAddr("192.168.50.7")
	table := routes{dst: netip.MustParseAddr("192.168.1.2")}
	asked := 0
	b.routes.src = func(a netip.Addr) (netip.Addr, error) { asked++; return table.src(a) }
	ep := &conn.StdNetEndpoint{AddrPort: netip.AddrPortFrom(dst, 20483)}
	send := func() error { return b.Send([][]byte{{1}}, ep) }

	if err := send(); err != nil {
		t.Fatal(err)
	}
	table[dst] = netip.MustParseAddr("100.64.0.4") // tailscale up --accept-routes
	now = now.Add(RouteTTL - time.Second)
	if err := send(); err != nil {
		t.Fatalf("within the TTL the cached route holds: %v", err)
	}
	now = now.Add(2 * time.Second)
	if err := send(); !errors.Is(err, errViaTailscale) {
		t.Fatalf("after the TTL a Tailscale route is refused: %v", err)
	}
	table[dst] = netip.MustParseAddr("192.168.1.2")
	now = now.Add(RouteTTL + time.Second)
	if err := send(); err != nil {
		t.Fatalf("the route went away: %v", err)
	}
	if asked != 3 {
		t.Errorf("asked the kernel %d times, want 3", asked)
	}
}
