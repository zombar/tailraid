package cluster

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// IsLive of this host's own disk reads the disk list alone: building the whole /health
// reply (mirrors, restore jobs, the applied index) once per slot of a status walk made
// the walk cost seconds at 100k chunks (docs/12 §8).
// Invariant: I-375
func TestProber_IsLive_OwnDiskReadsOnlyDisks(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	self, _ := n.SelfIdx()
	r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "sd", Name: "a/1", Kind: meta.KindDisk, Host: self, Capacity: 1}}})
	if err != nil || r.Err != nil {
		t.Fatal(err, r.Err)
	}
	disk := meta.NodeIdx(r.Ino)
	p := NewProber(n, &diskHealth{}, clock.NewFake(time.Now()))
	p.SelfHealth = func() rpc.Health { t.Fatal("IsLive built the whole health reply"); return rpc.Health{} }
	ok := true
	p.SelfDisks = func() []rpc.DiskHealth { return []rpc.DiskHealth{{ID: "sd", OK: ok}} }
	if !p.IsLive(disk) {
		t.Fatal("own open disk not live")
	}
	ok = false
	if p.IsLive(disk) {
		t.Fatal("own failing disk live")
	}
}

// SelfIdx answers from the node's own row without scanning the member table each time,
// and still follows the row: a node whose row is removed has no index.
// Invariant: I-375
func TestNode_SelfIdx_FollowsRow(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	first, ok := n.SelfIdx()
	if !ok || first == 0 {
		t.Fatalf("SelfIdx %d %v", first, ok)
	}
	for range 3 {
		if again, ok := n.SelfIdx(); !ok || again != first {
			t.Fatalf("SelfIdx changed: %d %v, want %d", again, ok, first)
		}
	}
	r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: first, State: meta.Removed, Now: 2}})
	if err != nil || r.Err != nil {
		t.Fatal(err, r.Err)
	}
	if idx, ok := n.SelfIdx(); ok {
		t.Fatalf("removed row still answers %d", idx)
	}
}

// BenchmarkProber_IsLive_OwnDisk is the per-slot cost of a status walk on a disk host.
func BenchmarkProber_IsLive_OwnDisk(b *testing.B) {
	n := bootstrap(b, b.TempDir())
	defer n.Close()
	self, _ := n.SelfIdx()
	r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "sd", Name: "a/1", Kind: meta.KindDisk, Host: self, Capacity: 1}}})
	if err != nil || r.Err != nil {
		b.Fatal(err, r.Err)
	}
	disk := meta.NodeIdx(r.Ino)
	p := NewProber(n, &diskHealth{}, clock.NewFake(time.Now()))
	p.SelfHealth = func() rpc.Health { return rpc.Health{Disks: []rpc.DiskHealth{{ID: "sd", OK: true}}} }
	p.SelfDisks = func() []rpc.DiskHealth { return []rpc.DiskHealth{{ID: "sd", OK: true}} }
	b.ResetTimer()
	for range b.N {
		if !p.IsLive(disk) {
			b.Fatal("not live")
		}
	}
}
