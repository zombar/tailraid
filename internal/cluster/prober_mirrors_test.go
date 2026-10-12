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

type mirrorHealth struct {
	down    map[string]bool
	mirrors map[string][]rpc.MirrorHealth
}

func (f *mirrorHealth) Health(_ context.Context, addr string) (rpc.Health, error) {
	if f.down[addr] {
		return rpc.Health{}, errors.New("down")
	}
	return rpc.Health{Mirrors: f.mirrors[addr]}, nil
}

// A mirror is online on every node exactly while its host is live and the host's last
// health reply lists it online (its marker names this cluster and this mirror); an
// unplugged one, one the host leaves out and one on an unreachable host are offline, and
// the last reply's use and the time it was last seen survive (docs/16 §1).
//
// Invariant: I-188
func TestMirror_OnlineFollowsMarkerAndHost(t *testing.T) {
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
	selfMirror := upsert(meta.Node{ID: "sm", Name: "a/m1", Kind: meta.KindMirror, Host: self})
	peerMirror := upsert(meta.Node{ID: "pm", Name: "b/m1", Kind: meta.KindMirror, Host: peer})
	silent := upsert(meta.Node{ID: "pm2", Name: "b/m2", Kind: meta.KindMirror, Host: peer})

	clk := clock.NewFake(time.Now())
	sync := &rpc.SyncHealth{ID: 7, Snapshot: "2026-10-12-0900Z", Read: 40, Total: 100, BytesNew: 12}
	fh := &mirrorHealth{down: map[string]bool{}, mirrors: map[string][]rpc.MirrorHealth{
		"10.0.0.2:1": {{ID: "pm", Online: true, Used: 1 << 30, Free: 3 << 30, Total: 4 << 30, Syncing: sync}},
	}}
	p := NewProber(n, fh, clk)
	selfMirrors := []rpc.MirrorHealth{{ID: "sm", Online: true, Used: 5}}
	p.SelfHealth = func() rpc.Health { return rpc.Health{Mirrors: selfMirrors} }
	p.Tick(ctx)
	for _, c := range []struct {
		idx    meta.NodeIdx
		online bool
	}{{selfMirror, true}, {peerMirror, true}, {silent, false}} {
		if p.MirrorOnline(c.idx) != c.online {
			t.Errorf("first tick: mirror %d online=%v, want %v", c.idx, !c.online, c.online)
		}
	}
	if mp, _ := p.Mirror(peerMirror); mp.Health.Used != 1<<30 || mp.Health.Syncing == nil || mp.Health.Syncing.Read != 40 || mp.LastSeen != clk.Now() {
		t.Fatalf("peer mirror probe = %+v", mp)
	}
	if mp, _ := p.Mirror(silent); mp.Health.Reason == "" {
		t.Fatal("a mirror its host leaves out has no reason")
	}

	// unplugged: the host lists it offline with the reason, at the next probe
	seen := clk.Now()
	fh.mirrors["10.0.0.2:1"][0] = rpc.MirrorHealth{ID: "pm", Online: false, Reason: "not attached: no marker"}
	selfMirrors[0].Online = false
	clk.Advance(ProbeInterval)
	p.Tick(ctx)
	if p.MirrorOnline(peerMirror) || p.MirrorOnline(selfMirror) {
		t.Fatal("an unplugged mirror is online")
	}
	if mp, _ := p.Mirror(peerMirror); mp.Health.Reason != "not attached: no marker" || mp.LastSeen != seen {
		t.Fatalf("unplugged probe = %+v, want the reason and last seen %v", mp, seen)
	}

	// plugged back in, then the host becomes unreachable: offline, the use kept
	fh.mirrors["10.0.0.2:1"][0] = rpc.MirrorHealth{ID: "pm", Online: true, Used: 2 << 30, Free: 2 << 30}
	clk.Advance(ProbeInterval)
	p.Tick(ctx)
	if !p.MirrorOnline(peerMirror) {
		t.Fatal("a mirror plugged back in is offline")
	}
	back := clk.Now()
	fh.down["10.0.0.2:1"] = true
	clk.Advance(ProbeInterval)
	p.Tick(ctx)
	if p.MirrorOnline(peerMirror) {
		t.Fatal("a mirror on an unreachable host is online")
	}
	if mp, _ := p.Mirror(peerMirror); mp.Health.Used != 2<<30 || mp.LastSeen != back || mp.Health.Syncing != nil {
		t.Fatalf("unreachable host's mirror = %+v", mp)
	}

	// a listed-online mirror whose host the probes no longer call live is offline even
	// before the next reply says otherwise
	fh.down["10.0.0.2:1"] = false
	p.Tick(ctx)
	if !p.MirrorOnline(peerMirror) {
		t.Fatal("not online after the host answered again")
	}
	fh.down["10.0.0.2:1"] = true
	clk.Advance(LiveThreshold + time.Second)
	p.mu.Lock()
	p.mirrors[peerMirror].Online = true // as if the last reply still stood
	p.mu.Unlock()
	if p.MirrorOnline(peerMirror) {
		t.Fatal("a mirror on a host past LiveThreshold is online")
	}
}
