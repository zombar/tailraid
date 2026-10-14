package admin

import (
	"net"
	"net/netip"
	"testing"
)

const netDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1234567    9000    0    0    0     0          0         0  1234567    9000    0    0    0     0       0          0
  eth0: 500000000 400000    0    0    0     0          0      1000 120000000 300000    0    0    0     0       0          0
tailscale0:  7000000   50000    0    0    0     0          0         0   3000000   40000    0    0    0     0       0          0
  junk: x y
  short: 1 2 3
`

// The /proc/net/dev parser sums every interface but loopback, picks out the tailnet
// interface by name, ignores malformed lines, and reports nothing for an empty listing.
// Invariant: I-190
func TestVitals_NetworkParsers_PerOS(t *testing.T) {
	for _, tc := range []struct {
		name, text, tailnet string
		rx, tx, trx, ttx    uint64
		ok                  bool
	}{
		{"fixture, tailscale0", netDevFixture, "tailscale0", 507000000, 123000000, 7000000, 3000000, true},
		{"fixture, no tailnet", netDevFixture, "", 507000000, 123000000, 0, 0, true},
		{"fixture, unknown tailnet", netDevFixture, "wg0", 507000000, 123000000, 0, 0, true},
		{"loopback only", "lo: 5 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0\n", "lo", 0, 0, 0, 0, false},
		{"empty", "", "", 0, 0, 0, 0, false},
		{"header only", netDevFixture[:120], "", 0, 0, 0, 0, false},
	} {
		rx, tx, trx, ttx, ok := parseNetDev(tc.text, tc.tailnet)
		if rx != tc.rx || tx != tc.tx || trx != tc.trx || ttx != tc.ttx || ok != tc.ok {
			t.Errorf("%s: got %d %d %d %d %v, want %d %d %d %d %v", tc.name, rx, tx, trx, ttx, ok, tc.rx, tc.tx, tc.trx, tc.ttx, tc.ok)
		}
	}
}

// The tailnet interface is found by the address it holds, the same way on every OS; the
// zero address and an address nobody holds give none.
// Invariant: I-190
func TestVitals_TailnetInterface_Match(t *testing.T) {
	if meshInterface(netip.Addr{}) != nil {
		t.Error("the zero address matched an interface")
	}
	if meshInterface(netip.MustParseAddr("192.0.2.1")) != nil {
		t.Error("a documentation address matched an interface")
	}
	lo := meshInterface(netip.MustParseAddr("127.0.0.1"))
	if lo == nil {
		t.Skip("no loopback address on this host")
	}
	if lo.Flags&net.FlagLoopback == 0 {
		t.Errorf("%s holds 127.0.0.1 but is not a loopback interface", lo.Name)
	}
}
