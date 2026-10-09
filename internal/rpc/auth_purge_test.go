package rpc

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/zombar/tailraid/internal/tailscale"
)

// blockingTS answers WhoIs only when released, and counts the calls.
type blockingTS struct {
	tailscale.Client
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingTS) WhoIs(ctx context.Context, ip netip.Addr) (*tailscale.WhoIs, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.Client.WhoIs(ctx, ip)
}

// A WhoIs that was in flight when the cache was purged does not land in the cache
// afterwards: the membership change that caused the purge may have changed what its
// answer means, so the next request looks the address up again.
//
// Invariant: I-18
func TestWhoIs_PurgeDuringLookupIsNotCached(t *testing.T) {
	ts := &blockingTS{Client: tailscale.NewFake(memberIP, "self-1", nil), entered: make(chan struct{}, 1), release: make(chan struct{})}
	a := &WhoIsAuthorizer{TS: ts}
	done := make(chan error, 1)
	go func() {
		_, err := a.Identify(ctx, memberIP)
		done <- err
	}()
	<-ts.entered // the lookup is inside WhoIs
	a.Purge()
	close(ts.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// without the purge this would be a cache hit; with it, WhoIs runs again
	ts.release = make(chan struct{})
	go func() {
		_, err := a.Identify(ctx, memberIP)
		done <- err
	}()
	<-ts.entered
	close(ts.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := ts.calls.Load(); n != 2 {
		t.Fatalf("%d WhoIs calls, want 2: the purged lookup was cached", n)
	}
	// and now it is cached
	if _, err := a.Identify(ctx, memberIP); err != nil {
		t.Fatal(err)
	}
	if n := ts.calls.Load(); n != 2 {
		t.Fatalf("%d WhoIs calls after a warm cache, want 2", n)
	}
}
