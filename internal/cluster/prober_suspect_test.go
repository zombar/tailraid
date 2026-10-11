package cluster

import (
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// A suspect mark excludes the host and its disks from Live() at once, survives a failing
// probe, and clears on the first probe of the host that succeeds; this host is never
// suspect of itself.
// Invariant: I-170
// Lifecycle: L-210
func TestProber_SuspectUntilNextProbe(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	defer n.Close()
	upsert := func(nd meta.Node) meta.NodeIdx {
		r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
		if err != nil || r.Err != nil {
			t.Fatal(err, r.Err)
		}
		return meta.NodeIdx(r.Ino)
	}
	peer := upsert(meta.Node{ID: "peer-b000000000000000", Name: "b", Kind: meta.KindDevice, Addr: "10.0.0.2", RPCPort: 1, RaftPort: 1, Capacity: 1})
	disk := upsert(meta.Node{ID: "pd1", Name: "b/1", Kind: meta.KindDisk, Host: peer, Capacity: 1})
	clk := clock.NewFake(time.Now())
	fh := &diskHealth{down: map[string]bool{}, disks: map[string][]rpc.DiskHealth{"10.0.0.2:1": {{ID: "pd1", OK: true}}}}
	p := NewProber(n, fh, clk)
	p.Tick(ctx)
	if !p.IsLive(peer) || !p.IsLive(disk) {
		t.Fatalf("peer or its disk not live after a good probe: %v", p.Live())
	}

	p.Suspect(disk) // a shard RPC to the disk failed: its host is suspect
	if p.IsLive(peer) || p.IsLive(disk) || !p.IsSuspect(peer) || !p.IsSuspect(disk) {
		t.Fatalf("suspect host still live: peer %v disk %v", p.IsLive(peer), p.IsLive(disk))
	}
	if live := p.Live(); slices.Contains(live, peer) || slices.Contains(live, disk) {
		t.Fatalf("Live() lists a suspect host: %v", live)
	}
	if p.DeadFor(peer) != 0 {
		t.Fatal("a suspect mark started the dead clock")
	}
	fh.down["10.0.0.2:1"] = true // a failing probe keeps the mark
	p.Tick(ctx)
	if !p.IsSuspect(peer) || p.IsLive(peer) {
		t.Fatal("a failing probe cleared the mark")
	}
	fh.down["10.0.0.2:1"] = false // the first good probe clears it
	clk.Advance(ProbeInterval)
	p.Tick(ctx)
	if p.IsSuspect(peer) || !p.IsLive(peer) || !p.IsLive(disk) {
		t.Fatalf("mark not cleared by a good probe: suspect %v live %v", p.IsSuspect(peer), p.IsLive(peer))
	}

	self, _ := n.SelfIdx()
	p.Suspect(self)
	if p.IsSuspect(self) || !p.IsLive(self) {
		t.Fatal("this host marked itself suspect")
	}
}
