package cluster

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// A removed member's probe entry goes with it: the prober's map holds the live set, not
// every node the cluster ever had.
func TestProber_DropsRemovedNode(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{
		ID: "peer-gone-000000", Name: "gone", Kind: meta.KindDevice, Addr: "10.0.0.9", RPCPort: 1, RaftPort: 1, Capacity: 1,
	}}})
	if err != nil || r.Err != nil {
		t.Fatal(err, r.Err)
	}
	idx := meta.NodeIdx(r.Ino)
	clk := clock.NewFake(time.Now())
	p := NewProber(n, &fakeHealth{down: map[string]bool{}}, clk)
	p.Tick(ctx)
	if _, ok := p.Snapshot()[idx]; !ok {
		t.Fatal("no probe for the live peer")
	}
	if r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: idx, State: meta.Removed, Now: 1}}); err != nil || r.Err != nil {
		t.Fatal(err, r.Err)
	}
	p.Tick(ctx)
	if _, ok := p.Snapshot()[idx]; ok {
		t.Fatal("the removed peer still has a probe entry")
	}
}
