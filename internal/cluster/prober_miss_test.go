package cluster

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// One missed probe never makes a node dead: ticks are ProbeInterval apart after the
// slowest probe of the previous tick (up to the probe timeout), so with one miss the
// replies that succeed can be two tick spacings plus a timeout apart. Two consecutive
// misses do. On a link that bufferbloat slows past the timeout now and then, a node used
// to drop out for a few seconds at every slow probe, and every read through it failed.
// Invariant: I-313
func TestProber_OneMissedProbeKeepsLive(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	defer n.Close()
	nd := meta.Node{ID: "peer-b000000000000000", Name: "b", Kind: meta.KindDevice, Addr: "10.0.0.2", RPCPort: 1, RaftPort: 1, Capacity: 1}
	r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	if err != nil || r.Err != nil {
		t.Fatal(err, r.Err)
	}
	peer := meta.NodeIdx(r.Ino)
	clk := clock.NewFake(time.Now())
	fh := &diskHealth{down: map[string]bool{}, disks: map[string][]rpc.DiskHealth{}}
	p := NewProber(n, fh, clk)
	p.Tick(ctx)                          // a good probe at t, answered at once
	spacing := ProbeInterval + p.timeout // the most a tick can follow the previous one's start

	fh.down["10.0.0.2:1"] = true
	clk.Advance(spacing)
	p.Tick(ctx)                      // missed
	clk.Advance(spacing + p.timeout) // the next tick's good reply arrives at its timeout
	if !p.IsLive(peer) {
		t.Fatalf("dead %v after the last good probe, one probe missed", 2*spacing+p.timeout)
	}

	p.Tick(ctx) // a second consecutive miss
	clk.Advance(LiveThreshold - (2*spacing + p.timeout) + time.Second)
	if p.IsLive(peer) {
		t.Fatalf("still live %v after the last good probe, two probes missed", LiveThreshold+time.Second)
	}
}
