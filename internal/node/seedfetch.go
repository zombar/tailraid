package node

import (
	"context"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// SeedFetchEvery is how often a node without a leader asks the members it knows for
// their device rows (docs/08, I-282).
const SeedFetchEvery = 10 * time.Second

// seedFetch widens the authorizer's seed of a node that has no leader yet with the rows
// the members it knows hold: a member that joined while this node was down is in neither
// its log nor its seed file, and may be the leader (I-282).
type seedFetch struct {
	clk   clock.Clock
	self  string                                                      // this node's ID, never asked
	ask   func(ctx context.Context, addr string) ([]meta.Node, error) // rpc.Client.MemberList
	ready func() bool                                                 // a leader connected: its table takes over
	set   func(seed []meta.Node)                                      // the authorizer's seed and the seed file
}

// run asks every member of seed each round until the node is ready or ctx ends, and
// calls set whenever an answer named a member seed lacked. A row seed holds keeps its
// own record: an answer only ever adds.
func (f seedFetch) run(ctx context.Context, seed []meta.Node) {
	seed = append([]meta.Node(nil), seed...)
	for !f.ready() {
		known := map[string]bool{}
		for i := range seed {
			known[seed[i].ID] = true
		}
		asking := append([]meta.Node(nil), seed...)
		for i := range asking {
			m := &asking[i]
			if m.ID == f.self || m.Addr == "" || f.ready() {
				continue
			}
			rows, err := f.ask(ctx, m.RPCAddr())
			if err != nil {
				continue // down too, or not a member there yet: the next round asks again
			}
			added := false
			for j := range rows {
				if r := &rows[j]; r.Kind == meta.KindDevice && r.State != meta.Removed && r.Addr != "" && !known[r.ID] {
					known[r.ID] = true
					seed = append(seed, *r)
					added = true
				}
			}
			if added {
				f.set(append([]meta.Node(nil), seed...))
			}
		}
		t := f.clk.NewTimer(SeedFetchEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
	}
}
