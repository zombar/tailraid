package cluster

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// slowRig is a node with peers b, c, d… each holding one disk, all probed live, and a
// volume of k+m.
type slowRig struct {
	n     *Node
	p     *Prober
	clk   *clock.Fake
	peers []meta.NodeIdx
	disks []meta.NodeIdx
}

func newSlowRig(t *testing.T, peers int, k, m uint8) *slowRig {
	t.Helper()
	n := bootstrap(t, t.TempDir())
	t.Cleanup(func() { n.Close() })
	propose := func(cmd meta.Command) meta.Result {
		t.Helper()
		r, _, err := n.Propose(ctx, cmd)
		if err != nil || r.Err != nil {
			t.Fatal(err, r.Err)
		}
		return r
	}
	propose(meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: k, M: m, Now: 1}})
	r := &slowRig{n: n, clk: clock.NewFake(time.Now())}
	fh := &diskHealth{down: map[string]bool{}, disks: map[string][]rpc.DiskHealth{}}
	for i := range peers {
		addr := fmt.Sprintf("10.0.0.%d", i+2)
		peer := meta.NodeIdx(propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: fmt.Sprintf("peer-%c00000000000000000", 'b'+i), Name: string(rune('b' + i)), Kind: meta.KindDevice, Addr: addr, RPCPort: 1, RaftPort: 1, Capacity: 1}}}).Ino)
		id := fmt.Sprintf("d%d", i)
		disk := meta.NodeIdx(propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: id, Name: string(rune('b'+i)) + "/1", Kind: meta.KindDisk, Host: peer, Capacity: 1 << 40}}}).Ino)
		fh.disks[addr+":1"] = []rpc.DiskHealth{{ID: id, OK: true}}
		r.peers, r.disks = append(r.peers, peer), append(r.disks, disk)
	}
	r.p = NewProber(n, fh, r.clk)
	r.p.Tick(ctx)
	for _, d := range r.disks {
		if !r.p.IsLive(d) {
			t.Fatalf("disk %d not live after a good probe", d)
		}
	}
	return r
}

// advance moves the clock and probes again, as the prober's tick would: the peers stay live.
func (r *slowRig) advance(d time.Duration) {
	r.clk.Advance(d)
	r.p.Tick(ctx)
}

// A cut holds the host (and its disks) slow for SlowHold: it stays live, unlike a
// suspect host; a cut during the hold or within one hold's length after it doubles the
// next, up to SlowHoldMax; a hold's length with no cut resets it; and this host never
// holds itself.
// Invariant: I-371
// Lifecycle: L-222
func TestProber_SlowHold_DoublesCapsResets(t *testing.T) {
	r := newSlowRig(t, 6, 2, 1)
	p, peer, disk := r.p, r.peers[0], r.disks[0]
	if !p.Slow(disk) {
		t.Fatal("the hold was refused with spare targets")
	}
	if !p.Held(peer) || !p.Held(disk) || !p.IsLive(peer) || !p.IsLive(disk) || p.IsSuspect(peer) {
		t.Fatalf("held %v/%v live %v/%v suspect %v: want held, live, not suspect", p.Held(peer), p.Held(disk), p.IsLive(peer), p.IsLive(disk), p.IsSuspect(peer))
	}
	if got := p.SlowLeft(peer); got != SlowHold {
		t.Fatalf("held for %s, want %s", got, SlowHold)
	}
	r.advance(SlowHold)
	if p.Held(peer) {
		t.Fatal("still held after its hold ran out")
	}
	// the next cut within one hold's length after it: doubled, then doubled again while held, up to the cap
	want := SlowHold
	for range 6 {
		p.Slow(disk)
		want = min(2*want, SlowHoldMax)
		if got := p.SlowLeft(peer); got != want {
			t.Fatalf("held for %s, want %s", got, want)
		}
		r.advance(time.Second)
	}
	if want != SlowHoldMax {
		t.Fatalf("the doubling never reached the cap: %s", want)
	}
	// a hold's length with no cut after it ends: the next is SlowHold again
	r.advance(2 * SlowHoldMax)
	p.Slow(disk)
	if got := p.SlowLeft(peer); got != SlowHold {
		t.Fatalf("after a quiet hold: held for %s, want %s", got, SlowHold)
	}
	self, _ := r.n.SelfIdx()
	if p.Slow(self) || p.Held(self) {
		t.Fatal("this host held itself slow")
	}
}

// A hold is never taken when it would leave fewer than k+m live, unheld targets: with
// 2+1 and four disks the first slow host is held, the second is not; once the first
// hold runs out, the second can be held.
// Invariant: I-371
func TestProber_SlowHold_NeverBelowKPlusM(t *testing.T) {
	r := newSlowRig(t, 4, 2, 1)
	p := r.p
	if !p.Slow(r.disks[0]) || !p.Held(r.peers[0]) {
		t.Fatal("the first hold was refused with a spare target")
	}
	if p.Slow(r.disks[1]) || p.Held(r.peers[1]) {
		t.Fatal("a second hold left fewer than k+m targets")
	}
	r.advance(SlowHold)
	if !p.Slow(r.disks[1]) || !p.Held(r.peers[1]) {
		t.Fatal("the hold was refused once the first had run out")
	}
}

// A held host is ranked after every other device and before S3: a read reaches it only
// when it needs it.
// Invariant: I-371
func TestProber_Rank_SlowHostLastBeforeS3(t *testing.T) {
	r := newSlowRig(t, 4, 2, 1)
	r2, _, err := r.n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "s3-b2", Name: "b2", Kind: meta.KindS3, Capacity: 1 << 40, S3Endpoint: "https://s3", S3Bucket: "b"}}})
	if err != nil || r2.Err != nil {
		t.Fatal(err, r2.Err)
	}
	bucket := meta.NodeIdx(r2.Ino)
	r.p.Slow(r.disks[0])
	order := r.p.Rank([]meta.NodeIdx{bucket, r.disks[0], r.disks[1], r.disks[2], r.disks[3]})
	if order[len(order)-1] != bucket || order[len(order)-2] != r.disks[0] {
		t.Fatalf("rank %v: want the held disk %d just before the bucket %d", order, r.disks[0], bucket)
	}
	if slices.Index(order, r.disks[0]) < slices.Index(order, r.disks[3]) {
		t.Fatal("the held disk ranks before a healthy one")
	}
}
