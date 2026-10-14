package dev_test

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/dev"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// A drop rule cuts all four paths on the node holding it: Raft dials to the peer, Raft
// connections from the peer, outbound RPC to it and inbound RPC from it; clearing the
// rules restores them.
// Invariant: I-59
func TestFaults_DropRulesCutRaftAndRPC(t *testing.T) {
	var cid [16]byte
	cid[0] = 9
	faults := dev.NewFaults()
	// Raft stream: node A holds the rules, node B is the peer
	ta, err := cluster.NewTransport("127.0.0.1:0", "", cid, nil, faults, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ta.(*raft.NetworkTransport).Close()
	tb, err := cluster.NewTransport("127.0.0.1:0", "", cid, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.(*raft.NetworkTransport).Close()
	ping := func(from, to raft.Transport) error {
		var resp raft.AppendEntriesResponse
		return from.(*raft.NetworkTransport).AppendEntries("", to.LocalAddr(), &raft.AppendEntriesRequest{ProtocolVersion: raft.ProtocolVersionMax}, &resp)
	}
	faults.Set([]string{"127.0.0.1"})
	if err := ping(ta, tb); err == nil || !isDrop(err) {
		t.Fatalf("A→B dial with a rule: %v", err)
	}
	if err := ping(tb, ta); err == nil {
		t.Fatalf("B→A should be refused by A's accept")
	}
	faults.Set(nil)
	done := make(chan error, 1)
	go func() { done <- ping(ta, tb) }()
	select {
	case err := <-done:
		if err != nil && isDrop(err) {
			t.Fatalf("after heal: %v", err)
		}
	case <-time.After(3 * time.Second): // the handshake passed and raft waits for a consumer: fine
	}

	// RPC: a server with the rules answers 503 to the peer; a client with the rules refuses
	srv, err := rpc.NewServer(rpc.Options{
		NodeID: "a", ClusterID: cid, Applier: fakeApplier{}, Auth: rpc.AllowAll{},
		Members: func() []meta.Node { return nil }, Blocked: faults.Blocked,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	addr := hs.Listener.Addr().String()
	cl := rpc.NewClient(cid, nil)
	cl.WrapTransport(faults.Transport)
	ctx := context.Background()
	if _, err := cl.Health(ctx, addr); err != nil {
		t.Fatalf("health without rules: %v", err)
	}
	faults.Set([]string{"127.0.0.1"})
	if _, err := cl.Health(ctx, addr); err == nil || !strings.Contains(err.Error(), dev.ErrDropped.Error()) {
		t.Fatalf("outbound with a rule: %v", err)
	}
	plain := rpc.NewClient(cid, nil)
	if _, err := plain.Health(ctx, addr); err == nil {
		t.Fatal("inbound from a blocked peer answered")
	}
	faults.Set(nil)
	if _, err := cl.Health(ctx, addr); err != nil {
		t.Fatalf("after heal: %v", err)
	}
}

func isDrop(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "fault rule") || strings.Contains(err.Error(), "handshake read") || errors.Is(err, net.ErrClosed))
}

type fakeApplier struct{}

func (fakeApplier) Propose(context.Context, meta.Command) (meta.Result, uint64, error) {
	return meta.Result{}, 0, nil
}
func (fakeApplier) AppliedIndex() uint64  { return 0 }
func (fakeApplier) IsLeader() bool        { return true }
func (fakeApplier) LeaderRPCAddr() string { return "" }
func (fakeApplier) Ready() bool           { return true }

// A rule that lands after a Raft connection is up fails that connection: the node holding
// the rule cuts a link it had open (a leader's replication pipelines) without the other
// side's help, and a heal lets a new dial through.
// Invariant: I-59
func TestFaults_RuleFailsOpenRaftConnection(t *testing.T) {
	var cid [16]byte
	cid[0] = 9
	faults := dev.NewFaults()
	ta, err := cluster.NewTransport("127.0.0.1:0", "", cid, nil, faults, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ta.(*raft.NetworkTransport).Close()
	// B listens on a port A can name: the transport advertises its bind address
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrB := l.Addr().String()
	_ = l.Close()
	tb, err := cluster.NewTransport(addrB, "", cid, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.(*raft.NetworkTransport).Close()
	// B answers every request, so A's calls complete and keep their pooled connection;
	// Close leaves pooled connections alone, so they are closed first (B's handler
	// goroutines end with them)
	defer ta.(*raft.NetworkTransport).CloseStreams()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case r := <-tb.Consumer():
				r.Respond(&raft.AppendEntriesResponse{Success: true}, nil)
			case <-stop:
				return
			}
		}
	}()
	ping := func() error {
		var resp raft.AppendEntriesResponse
		return ta.(*raft.NetworkTransport).AppendEntries("", raft.ServerAddress(addrB), &raft.AppendEntriesRequest{ProtocolVersion: raft.ProtocolVersionMax}, &resp)
	}
	if err := ping(); err != nil {
		t.Fatalf("ping without rules: %v", err)
	}
	faults.Set([]string{"127.0.0.1"})
	if err := ping(); err == nil || !isDrop(err) {
		t.Fatalf("ping over the open connection with a rule: %v", err)
	}
	faults.Set(nil)
	testharness.Eventually(t, func() bool { return ping() == nil }, "no ping after the heal")
}
