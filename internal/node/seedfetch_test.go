package node

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A node restarted while a member joined holds neither that member in its log nor in its
// seed. Until a leader connects it asks the members it knows, every SeedFetchEvery, and
// widens its seed with the rows they hold that it does not: the late joiner is accepted.
// A row it holds keeps its own record, itself is never asked, a member that does not
// answer is retried, and the asking stops once the node is ready.
//
// Invariant: I-282
func TestSeedFetch_AddsMembersAPeerKnows(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	self := meta.Node{Idx: 1, ID: "self", Kind: meta.KindDevice, Addr: "100.64.0.1", RPCPort: 20481}
	known := meta.Node{Idx: 2, ID: "known", Kind: meta.KindDevice, Addr: "100.64.0.2", TSStableID: "ts-known", RPCPort: 20481}
	late := meta.Node{Idx: 3, ID: "late", Kind: meta.KindDevice, Addr: "100.64.0.3", TSStableID: "ts-late", RPCPort: 20481}

	asked := make(chan string, 16)
	var up, ready atomic.Bool
	sets := make(chan []meta.Node, 4)
	f := seedFetch{
		clk:  clk,
		self: self.ID,
		ask: func(_ context.Context, addr string) ([]meta.Node, error) {
			asked <- addr
			if !up.Load() {
				return nil, errors.New("unreachable")
			}
			moved := known
			moved.Addr = "100.64.0.99" // its own record of a row this node holds is not taken
			return []meta.Node{self, moved, late}, nil
		},
		ready: ready.Load,
		set:   func(seed []meta.Node) { sets <- seed },
	}
	ctx := t.Context()
	done := make(chan struct{})
	go func() { f.run(ctx, []meta.Node{self, known}); close(done) }()

	if got := <-asked; got != known.RPCAddr() {
		t.Fatalf("asked %s first, want %s (never itself)", got, known.RPCAddr())
	}
	select {
	case s := <-sets:
		t.Fatalf("a member that did not answer widened the seed: %+v", s)
	default:
	}
	up.Store(true)
	testharness.Eventually(t, func() bool { return clk.Pending() > 0 }, "the loop arms its timer")
	clk.Advance(SeedFetchEvery)
	if got := <-asked; got != known.RPCAddr() {
		t.Fatalf("retried %s, want %s", got, known.RPCAddr())
	}
	seed := <-sets
	byID := map[string]meta.Node{}
	for _, m := range seed {
		byID[m.ID] = m
	}
	if len(seed) != 3 || byID["late"].TSStableID != "ts-late" || byID["known"].Addr != known.Addr {
		t.Fatalf("seed after the answer: %+v", seed)
	}
	ready.Store(true)
	testharness.Eventually(t, func() bool { return clk.Pending() > 0 }, "the loop arms its timer")
	clk.Advance(SeedFetchEvery)
	<-done
}
