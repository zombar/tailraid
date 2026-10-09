package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

type diskHealth struct {
	down  map[string]bool
	disks map[string][]rpc.DiskHealth
}

func (f *diskHealth) Health(_ context.Context, addr string) (rpc.Health, error) {
	if f.down[addr] {
		return rpc.Health{}, errors.New("down")
	}
	return rpc.Health{Free: 1, Disks: f.disks[addr]}, nil
}

// Invariant: I-90
func TestProber_DiskLiveness(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	defer n.Close()
	upsert := func(nd meta.Node) meta.NodeIdx {
		t.Helper()
		r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
		if err != nil || r.Err != nil {
			t.Fatal(err, r.Err)
		}
		return meta.NodeIdx(r.Ino)
	}
	self, _ := n.SelfIdx()
	peer := upsert(meta.Node{ID: "peer-b0000000000000", Name: "b", Kind: meta.KindDevice, Addr: "10.0.0.2", RPCPort: 1, RaftPort: 1, Capacity: 1})
	selfDisk := upsert(meta.Node{ID: "sd", Name: "a/1", Kind: meta.KindDisk, Host: self, Capacity: 1})
	peerDisk := upsert(meta.Node{ID: "pd1", Name: "b/1", Kind: meta.KindDisk, Host: peer, Capacity: 1})
	closed := upsert(meta.Node{ID: "pd2", Name: "b/2", Kind: meta.KindDisk, Host: peer, Capacity: 1})

	clk := clock.NewFake(time.Now())
	fh := &diskHealth{down: map[string]bool{}, disks: map[string][]rpc.DiskHealth{
		"10.0.0.2:1": {{ID: "pd1", OK: true}, {ID: "pd2", OK: false, Reason: "disk_id missing"}},
	}}
	p := NewProber(n, fh, clk)
	selfDisks := []rpc.DiskHealth{{ID: "sd", OK: true}}
	p.SelfHealth = func() rpc.Health { return rpc.Health{Disks: selfDisks} }
	p.Tick(ctx)
	for _, c := range []struct {
		idx  meta.NodeIdx
		live bool
	}{{self, true}, {peer, true}, {selfDisk, true}, {peerDisk, true}, {closed, false}} {
		if p.IsLive(c.idx) != c.live {
			t.Errorf("first tick: target %d live=%v, want %v", c.idx, !c.live, c.live)
		}
	}
	// a disk its host reports failing is dead at the next probe, while the host and its
	// other disks stay live
	fh.disks["10.0.0.2:1"][0].OK = false
	selfDisks[0].OK = false
	clk.Advance(ProbeInterval)
	p.Tick(ctx)
	if p.IsLive(peerDisk) || p.IsLive(selfDisk) {
		t.Fatalf("failing disks still live")
	}
	if !p.IsLive(peer) || !p.IsLive(self) {
		t.Fatalf("hosts went dead with their disks")
	}
	// its slots are re-placed after fill_after: MemberDown for the disk
	clk.Advance(FillAfter + time.Second)
	p.Tick(ctx)
	downs := map[meta.NodeIdx]bool{}
	for {
		select {
		case e := <-p.Events():
			if e.Kind == MemberDown {
				downs[e.Idx] = true
			}
			continue
		default:
		}
		break
	}
	if !downs[peerDisk] || !downs[selfDisk] || downs[peer] {
		t.Fatalf("MemberDown for %v", downs)
	}
	// the disk returns
	fh.disks["10.0.0.2:1"][0].OK = true
	p.Tick(ctx)
	if !p.IsLive(peerDisk) {
		t.Fatal("returned disk not live")
	}
	// a dead host takes its disks with it
	fh.down["10.0.0.2:1"] = true
	clk.Advance(LiveThreshold + time.Second)
	p.Tick(ctx)
	if p.IsLive(peer) || p.IsLive(peerDisk) {
		t.Fatal("disk of a dead host is live")
	}
	// Live lists live targets, disks included
	live := map[meta.NodeIdx]bool{}
	for _, idx := range p.Live() {
		live[idx] = true
	}
	if !live[self] || live[peerDisk] || live[closed] {
		t.Fatalf("Live() = %v", p.Live())
	}
}
