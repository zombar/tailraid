package cluster

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// Lifecycle: L-47
// Invariant: I-49
func TestIDStream_RejectsForeignCluster(t *testing.T) {
	mk := func(id byte) (*idStream, string) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return &idStream{StreamLayer: &tcpStream{l: l, advertise: l.Addr()}, id: [16]byte{id}}, l.Addr().String()
	}
	a, _ := mk(1)
	b, baddr := mk(1)
	c, _ := mk(2)
	accepted := make(chan error, 2)
	go func() {
		conn, err := b.Accept() // skips the foreign dialer, accepts the matching one
		if err == nil {
			conn.Close()
		}
		accepted <- err
	}()
	if _, err := c.Dial(raft.ServerAddress(baddr), time.Second); !errors.Is(err, ErrHandshakeRefused) { // the acceptor reads first and closes: the dialer never learns why
		t.Fatalf("foreign dial: %v", err)
	}
	conn, err := a.Dial(raft.ServerAddress(baddr), time.Second)
	if err != nil {
		t.Fatalf("same-cluster dial: %v", err)
	}
	conn.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept did not return for the matching peer")
	}
}

type fakeHealth struct {
	down  map[string]bool
	codec map[string]uint8 // the codec each peer reports; absent = 0, a binary that predates the field
}

func (f *fakeHealth) Health(_ context.Context, addr string) (rpc.Health, error) {
	if f.down[addr] {
		return rpc.Health{}, errors.New("down")
	}
	return rpc.Health{Codec: f.codec[addr]}, nil
}

// MinCodec is the oldest codec a live active device reports: a dead member does not
// count, a member that predates the field counts as 0, and self counts as this binary.
//
// Invariant: I-156
func TestProber_MinCodec(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	defer n.Close()
	for i, name := range []string{"b", "c"} {
		r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{
			ID: "peer-" + name + "000000000000", Name: name, Kind: meta.KindDevice, Addr: "10.0.0." + string(rune('2'+i)), RPCPort: 1, RaftPort: 1, Capacity: 1,
		}}})
		if err != nil || r.Err != nil {
			t.Fatal(err, r.Err)
		}
	}
	clk := clock.NewFake(time.Now())
	fh := &fakeHealth{down: map[string]bool{}, codec: map[string]uint8{"10.0.0.2:1": meta.CodecVersion, "10.0.0.3:1": 3}}
	p := NewProber(n, fh, clk)
	p.Tick(ctx)
	if got := p.MinCodec(); got != 3 {
		t.Fatalf("min codec %d with a codec-3 member, want 3", got)
	}
	// the old member dies: it no longer holds the cluster back
	fh.down["10.0.0.3:1"] = true
	clk.Advance(LiveThreshold + time.Second)
	p.Tick(ctx)
	if got := p.MinCodec(); got != meta.CodecVersion {
		t.Fatalf("min codec %d with the old member dead, want %d", got, meta.CodecVersion)
	}
	// it returns updated; a member that reports no codec at all is the oldest of all
	fh.down["10.0.0.3:1"] = false
	fh.codec["10.0.0.3:1"] = meta.CodecVersion
	delete(fh.codec, "10.0.0.2:1")
	p.Tick(ctx)
	if got := p.MinCodec(); got != 0 {
		t.Fatalf("min codec %d with a member that predates the field, want 0", got)
	}
}

// Lifecycle: L-86, L-87, L-88, L-92, L-131, L-132
func TestProber_LivenessAndHoldDowns(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	defer n.Close()
	// three device members: self (idx 1) plus two peers with fake RPC addresses
	for i, name := range []string{"b", "c"} {
		r, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{
			ID: "peer-" + name + "000000000000", Name: name, Kind: meta.KindDevice, Addr: "10.0.0." + string(rune('2'+i)), RPCPort: 1, RaftPort: 1, Capacity: 1,
		}}})
		if err != nil || r.Err != nil {
			t.Fatal(err, r.Err)
		}
	}
	clk := clock.NewFake(time.Now())
	fh := &fakeHealth{down: map[string]bool{}}
	p := NewProber(n, fh, clk)
	p.Tick(ctx)
	if live := p.Live(); len(live) != 3 {
		t.Fatalf("live %v", live)
	}
	if r := p.Rank([]meta.NodeIdx{3, 2, 1}); r[0] != 1 || len(r) != 3 {
		t.Fatalf("rank %v (self first)", r)
	}
	// node 3 dies: still live until the threshold, then dead; MemberDown only after fill_after
	fh.down["10.0.0.3:1"] = true
	p.Tick(ctx)
	if !p.IsLive(3) {
		t.Fatal("dead before LiveThreshold")
	}
	clk.Advance(LiveThreshold + time.Second)
	p.Tick(ctx)
	if p.IsLive(3) || len(p.Live()) != 2 {
		t.Fatalf("still live after threshold: %v", p.Live())
	}
	clk.Advance(FillAfter - time.Second)
	p.Tick(ctx)
	select {
	case e := <-p.Events():
		t.Fatalf("premature event %+v", e)
	default:
	}
	clk.Advance(2 * time.Second)
	p.Tick(ctx)
	select {
	case e := <-p.Events():
		if e.Kind != MemberDown || e.Idx != 3 {
			t.Fatalf("event %+v", e)
		}
	default:
		t.Fatal("no MemberDown after fill_after")
	}
	if d := p.DeadFor(3); d < FillAfter {
		t.Fatalf("dead for %s", d)
	}
	// it returns: MemberUp, live again, no further events
	fh.down["10.0.0.3:1"] = false
	p.Tick(ctx)
	select {
	case e := <-p.Events():
		if e.Kind != MemberUp || e.Idx != 3 {
			t.Fatalf("event %+v", e)
		}
	default:
		t.Fatal("no MemberUp")
	}
	if !p.IsLive(3) || p.DeadFor(3) != 0 {
		t.Fatal("not live after return")
	}
	// long absence: MemberLost after evict_after
	fh.down["10.0.0.3:1"] = true
	clk.Advance(LiveThreshold + time.Second)
	p.Tick(ctx)
	clk.Advance(EvictAfter)
	p.Tick(ctx)
	kinds := map[EventKind]bool{}
	for len(p.Events()) > 0 {
		kinds[(<-p.Events()).Kind] = true
	}
	if !kinds[MemberDown] || !kinds[MemberLost] {
		t.Fatalf("events %v", kinds)
	}
	// health report on a single leader
	h := n.Health(clk, p)
	if h.State != Healthy || h.Voters != 1 || h.LiveVoters != 1 {
		t.Fatalf("health %+v", h)
	}
}
