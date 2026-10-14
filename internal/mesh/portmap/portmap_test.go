package portmap

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

var ctx = context.Background()

// fakeGateway is a responder on the loopback, and the Gateway function a mapper uses to
// find it.
func fakeGateway(t *testing.T, external string, clk clock.Clock, lifetime time.Duration, apply func(Mapping, bool) error) (*Responder, netip.AddrPort) {
	t.Helper()
	r := &Responder{External: netip.MustParseAddr(external), Apply: apply, Clock: clk, Lifetime: lifetime}
	ap, err := r.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, ap
}

// Both protocols map, refresh and release against the responder; PCP is tried first and
// NAT-PMP answers when PCP does not; the epoch counts from the responder's start.
// Invariant: I-330
func TestClients_MapRefreshRelease(t *testing.T) {
	var mu sync.Mutex
	var applied []string
	r, gw := fakeGateway(t, "203.0.113.7", clock.NewFake(time.Now()), 0, func(m Mapping, add bool) error {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, m.Protocol+":"+m.Endpoint().String()+":"+map[bool]string{true: "add", false: "del"}[add])
		return nil
	})
	local := netip.MustParseAddr("127.0.0.1")
	for _, c := range []Client{NewPCP(gw.Addr(), local, gw.Port()), NewNATPMP(gw.Addr(), gw.Port())} {
		_, epoch, err := c.Probe(ctx)
		if err != nil || epoch > 5 {
			t.Fatalf("%s probe: epoch %d, %v", c.Name(), epoch, err)
		}
		m, err := c.Map(ctx, 20483, 0, time.Hour)
		if err != nil || m.Protocol != c.Name() || m.External != 20483 || m.ExternalIP != r.External || m.Lifetime != time.Hour || m.Internal != 20483 {
			t.Fatalf("%s map: %+v %v", c.Name(), m, err)
		}
		again, err := c.Map(ctx, 20483, m.External, time.Hour)
		if err != nil || again.External != m.External {
			t.Fatalf("%s refresh: %+v %v", c.Name(), again, err)
		}
		if n := len(r.Mappings()); n != 1 {
			t.Fatalf("%s: %d mappings after a refresh, want 1", c.Name(), n)
		}
		if err := c.Release(ctx, m); err != nil {
			t.Fatalf("%s release: %v", c.Name(), err)
		}
		if n := len(r.Mappings()); n != 0 {
			t.Fatalf("%s: %d mappings after the release", c.Name(), n)
		}
	}
	mu.Lock()
	got := strings.Join(applied, " ")
	mu.Unlock()
	if want := []string{"pcp:203.0.113.7:20483:add", "pcp:203.0.113.7:20483:del", "natpmp:203.0.113.7:20483:add", "natpmp:203.0.113.7:20483:del"}; got != strings.Join(want, " ") {
		t.Fatalf("applied %v, want %v", got, want)
	}
	// a taken external port moves the next mapping up by one (another internal port of
	// the same host here: the responder keys a client by its source and internal port)
	a := NewNATPMP(gw.Addr(), gw.Port())
	first, _ := a.Map(ctx, 20483, 0, time.Hour)
	second, err := a.Map(ctx, 20484, first.External, time.Hour)
	if err != nil || second.External != first.External+1 || second.Internal != 20484 {
		t.Fatalf("second mapping: %+v %v (first %d)", second, err, first.External)
	}
	// discovery: PCP first; with the responder silent, neither
	c, _, _, err := Discover(ctx, gw.Addr(), local, gw.Port())
	if err != nil || c.Name() != "pcp" {
		t.Fatalf("discover: %v %v", c, err)
	}
	r.SetOff(true)
	if _, _, _, err := Discover(ctx, gw.Addr(), local, gw.Port()); err == nil {
		t.Fatal("a silent gateway was discovered")
	}
}

// The mapper holds one mapping: made at its first tick, refreshed at half its lifetime,
// made again when the gateway's epoch shows a restart, dropped when the gateway stops
// answering and retried every RetryEvery, released at Close.
// Invariant: I-330
func TestMapper_Lifecycle(t *testing.T) {
	clk := clock.NewFake(time.Now())
	r, gw := fakeGateway(t, "198.51.100.9", clk, 10*time.Minute, nil)
	m := &Mapper{Port: 20483, Clock: clk, GatewayPort: gw.Port(), Gateway: func(context.Context) (netip.Addr, netip.Addr, error) {
		return gw.Addr(), netip.MustParseAddr("127.0.0.1"), nil
	}}
	if _, ok := m.External(); ok {
		t.Fatal("an external endpoint before the first tick")
	}
	m.Tick(ctx)
	ext, ok := m.External()
	if !ok || ext.String() != "198.51.100.9:20483" {
		t.Fatalf("after the first tick: %s %v (%+v)", ext, ok, m.Status())
	}
	if st := m.Status(); st.Protocol != "pcp" || st.Gateway != gw.Addr().String() || st.Expires != "10m0s" {
		t.Fatalf("status %+v", st)
	}
	// half the lifetime: the refresh keeps the endpoint and pushes the expiry
	clk.Advance(5*time.Minute + time.Second)
	m.Tick(ctx)
	if st := m.Status(); st.Expires != "10m0s" || st.External != ext.String() {
		t.Fatalf("after the refresh: %+v", st)
	}
	if n := len(r.Mappings()); n != 1 {
		t.Fatalf("%d mappings after a refresh", n)
	}
	// the router restarts: its epoch starts over and the table is empty; the next
	// check maps again
	r.Reset()
	if n := len(r.Mappings()); n != 0 {
		t.Fatalf("%d mappings after the reset", n)
	}
	clk.Advance(CheckEvery + time.Second)
	m.Tick(ctx)
	if n := len(r.Mappings()); n != 1 {
		t.Fatalf("%d mappings after the gateway restarted: the mapper did not map again", n)
	}
	if st := m.Status(); st.Protocol != "pcp" || st.Since != "0s" {
		t.Fatalf("status after the re-map: %+v", st)
	}
	// the gateway goes silent: the next check drops the mapping, and the mapper retries
	// every RetryEvery until it answers again
	r.SetOff(true)
	clk.Advance(CheckEvery + time.Second)
	m.Tick(ctx)
	if _, ok := m.External(); ok {
		t.Fatal("a mapping held with the gateway silent")
	}
	if st := m.Status(); st.Protocol != "none" || st.Why == "" {
		t.Fatalf("status with the gateway silent: %+v", st)
	}
	m.Tick(ctx) // the retry: still silent
	r.SetOff(false)
	m.Tick(ctx) // not due
	if _, ok := m.External(); ok {
		t.Fatal("retried before RetryEvery")
	}
	clk.Advance(RetryEvery)
	m.Tick(ctx)
	if _, ok := m.External(); !ok {
		t.Fatalf("not mapped again after RetryEvery: %+v", m.Status())
	}
	m.Close(ctx)
	if n := len(r.Mappings()); n != 0 {
		t.Fatalf("%d mappings after Close", n)
	}
	if _, ok := m.External(); ok {
		t.Fatal("an external endpoint after Close")
	}
	// no gateway: nothing held, the reason reported
	none := &Mapper{Port: 20483, Clock: clk, Gateway: func(context.Context) (netip.Addr, netip.Addr, error) { return netip.Addr{}, netip.Addr{}, ErrNoGateway }}
	none.Tick(ctx)
	if st := none.Status(); st.Protocol != "none" || !strings.Contains(st.Why, "no default gateway") {
		t.Fatalf("no gateway: %+v", st)
	}
}

// The Linux routing table names the gateway of the default route, little-endian.
// Invariant: I-330
func TestParseProcRoute_Table(t *testing.T) {
	const table = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0000FEA9\t00000000\t0001\t0\t0\t1000\t0000FFFF\t0\t0\t0\n" +
		"eth0\t00000000\t020131AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	gw, err := parseProcRoute(strings.NewReader(table))
	if err != nil || gw.String() != "172.49.1.2" {
		t.Fatalf("gateway %s, %v", gw, err)
	}
	if _, err := parseProcRoute(strings.NewReader("Iface\tDestination\tGateway\neth0\t0000FEA9\t00000000\t0001\n")); err == nil {
		t.Fatal("a table without a default route gave a gateway")
	}
}
