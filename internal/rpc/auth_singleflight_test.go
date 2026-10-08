package rpc

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/tailscale"
)

// gatedTS blocks WhoIs until released and counts the calls.
type gatedTS struct {
	tailscale.Client
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (g *gatedTS) WhoIs(ctx context.Context, ip netip.Addr) (*tailscale.WhoIs, error) {
	g.calls.Add(1)
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.Client.WhoIs(ctx, ip)
}

// Concurrent cache misses for one address share a single WhoIs call; the answer is
// cached for all of them and the next miss, after expiry, calls again.
func TestWhoIs_ConcurrentMissesShareOneCall(t *testing.T) {
	ip := netip.MustParseAddr("100.64.0.9")
	g := &gatedTS{Client: tailscale.NewFake(netip.MustParseAddr("100.64.0.1"), "self", map[netip.Addr]string{ip: "peer-9"}), entered: make(chan struct{}), release: make(chan struct{})}
	a := &WhoIsAuthorizer{TS: g, Member: func(string) (meta.Node, bool) { return meta.Node{}, false }}
	const callers = 6
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			id, err := a.Identify(context.Background(), ip)
			ids <- id
			errs <- err
		}()
	}
	<-g.entered // one call is in flight; the others must be waiting for it
	close(g.release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if id := <-ids; id != "peer-9" {
			t.Fatalf("id %q", id)
		}
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("%d WhoIs calls for %d concurrent callers", n, callers)
	}
	if id, err := a.Identify(context.Background(), ip); err != nil || id != "peer-9" || g.calls.Load() != 1 {
		t.Fatalf("cached: %q %v calls=%d", id, err, g.calls.Load())
	}
}
