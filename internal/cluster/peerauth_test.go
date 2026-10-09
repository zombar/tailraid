package cluster

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// peerGate is a PeerAuth that admits everyone or no one.
type peerGate struct{ allow atomic.Bool }

func (g *peerGate) Allow(context.Context, netip.Addr) (meta.Node, error) {
	if g.allow.Load() {
		return meta.Node{Idx: 1}, nil
	}
	return meta.Node{}, errors.New("not a member")
}

// The acceptor asks the authorizer before it reads or writes a byte: a non-member is
// closed without learning the cluster ID, and a member that stays silent is sent nothing
// (the acceptor reads first). Once the gate opens the same dialer gets through.
//
// Invariant: I-92
func TestIDStream_RefusesNonMemberBeforeHandshake(t *testing.T) {
	gate := &peerGate{}
	mk := func(auth PeerAuth) (*idStream, string) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return &idStream{StreamLayer: &tcpStream{l: l, advertise: l.Addr()}, id: [16]byte{7}, auth: auth}, l.Addr().String()
	}
	dialer, _ := mk(nil)
	acceptor, addr := mk(gate)
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := acceptor.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() { acceptor.Close() })

	// gate closed: the dial fails at the handshake (the acceptor closed the socket)
	if _, err := dialer.Dial(raft.ServerAddress(addr), time.Second); err == nil {
		t.Fatal("a non-member completed the handshake")
	}
	select {
	case <-accepted:
		t.Fatal("a non-member connection reached Raft")
	case <-time.After(200 * time.Millisecond):
	}

	// gate open but the peer stays silent: the acceptor sends nothing first
	gate.allow.Store(true)
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := raw.Read(make([]byte, 16)); n != 0 || err == nil {
		t.Fatalf("the acceptor wrote %d bytes before the peer identified itself (err %v)", n, err)
	}
	raw.Close()

	// gate open and a real dialer: through
	conn, err := dialer.Dial(raft.ServerAddress(addr), time.Second)
	if err != nil {
		t.Fatalf("member dial: %v", err)
	}
	conn.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the member's connection was not accepted")
	}
}

// A re-join under a known node ID must present that record's tailnet identity: node IDs
// are public (/health), the StableID is what WhoIs vouched for.
//
// Invariant: I-94
func TestJoin_RejoinRequiresSameStableID(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	req := func(stable string) meta.Node {
		return meta.Node{ID: "node-two-0000000", TSStableID: stable, Addr: "10.0.0.2", RaftPort: 1, RPCPort: 2, Capacity: 1}
	}
	first := req("ts-a")
	if _, err := n.Join(ctx, joinRequest(first)); err != nil {
		t.Fatalf("first join: %v", err)
	}
	if _, err := n.Join(ctx, joinRequest(req("ts-b"))); !errors.Is(err, ErrBadMemberOp) {
		t.Fatalf("re-join with another identity: %v, want ErrBadMemberOp", err)
	}
	if _, err := n.Join(ctx, joinRequest(first)); err != nil {
		t.Fatalf("re-join with the same identity: %v", err)
	}
	var seen int
	n.FSM.ForEachNode(func(cur meta.Node) bool {
		if cur.ID == first.ID {
			seen++
			if cur.TSStableID != "ts-a" {
				t.Fatalf("record identity %q", cur.TSStableID)
			}
		}
		return true
	})
	if seen != 1 {
		t.Fatalf("%d records for the node, want 1", seen)
	}
}

func joinRequest(nd meta.Node) rpc.JoinRequest {
	return rpc.JoinRequest{NodeID: nd.ID, Name: nd.Name, TSStableID: nd.TSStableID, Addr: nd.Addr, RaftPort: nd.RaftPort, RPCPort: nd.RPCPort}
}
