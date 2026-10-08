package dev_test

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/dev"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
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
	ta, err := cluster.NewTransport("127.0.0.1:0", cid, nil, faults)
	if err != nil {
		t.Fatal(err)
	}
	defer ta.(*raft.NetworkTransport).Close()
	tb, err := cluster.NewTransport("127.0.0.1:0", cid, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.(*raft.NetworkTransport).Close()
	ping := func(from, to raft.Transport) error {
		var resp raft.AppendEntriesResponse
		return from.(*raft.NetworkTransport).AppendEntries("", to.LocalAddr(), &raft.AppendEntriesRequest{RPCHeader: raft.RPCHeader{ProtocolVersion: raft.ProtocolVersionMax}}, &resp)
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
	store, err := chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "shards"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := rpc.NewServer(rpc.Options{NodeID: "a", ClusterID: cid, Store: store, Applier: fakeApplier{}, Auth: rpc.AllowAll{},
		Members: func() []meta.Node { return nil }, Blocked: faults.Blocked})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	addr := hs.Listener.Addr().String()
	cl := rpc.NewClient(cid, nil, nil)
	cl.WrapTransport(faults.Transport)
	ctx := context.Background()
	if _, err := cl.Health(ctx, addr); err != nil {
		t.Fatalf("health without rules: %v", err)
	}
	faults.Set([]string{"127.0.0.1"})
	if _, err := cl.Health(ctx, addr); err == nil || !strings.Contains(err.Error(), dev.ErrDropped.Error()) {
		t.Fatalf("outbound with a rule: %v", err)
	}
	plain := rpc.NewClient(cid, nil, nil)
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
