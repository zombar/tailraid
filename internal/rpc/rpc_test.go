package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/tailscale"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"), goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"))
}

var ctx = context.Background()

// fakeApplier is a local FSM behind the Applier interface, with switchable leadership.
type fakeApplier struct {
	mu         sync.Mutex
	fsm        *meta.FSM
	idx        uint64
	leader     bool
	leaderAddr string
}

func newApplier() *fakeApplier {
	a := &fakeApplier{fsm: meta.NewFSM(), leader: true}
	a.apply(meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{ChunkSize: 1 << 16, K: 2, M: 1, Now: 1}})
	return a
}

func (a *fakeApplier) apply(c meta.Command) (meta.Result, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.idx++
	res := a.fsm.Apply(&raft.Log{Index: a.idx, Type: raft.LogCommand, Data: meta.EncodeCommand(c)}).(meta.Result)
	return res, a.idx
}

func (a *fakeApplier) Propose(_ context.Context, c meta.Command) (meta.Result, uint64, error) {
	if !a.IsLeader() {
		return meta.Result{}, 0, ErrNotLeader
	}
	r, i := a.apply(c)
	return r, i, nil
}
func (a *fakeApplier) IsLeader() bool        { a.mu.Lock(); defer a.mu.Unlock(); return a.leader }
func (a *fakeApplier) LeaderRPCAddr() string { a.mu.Lock(); defer a.mu.Unlock(); return a.leaderAddr }
func (a *fakeApplier) AppliedIndex() uint64  { return a.fsm.Applied() }
func (a *fakeApplier) setLeader(l bool, addr string) {
	a.mu.Lock()
	a.leader, a.leaderAddr = l, addr
	a.mu.Unlock()
}

type fakeFence struct{ placed map[chunk.ShardKey]bool }

func (f fakeFence) PlacedOnSelf(k chunk.ShardKey) (bool, bool) { return f.placed[k], true }

type fakeJoiner struct {
	mu  sync.Mutex
	app *fakeApplier
	ops []MemberOp
}

func (j *fakeJoiner) Join(_ context.Context, req JoinRequest) (JoinResponse, error) {
	if !j.app.IsLeader() {
		return JoinResponse{}, ErrNotLeader
	}
	res, _ := j.app.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{
		ID: req.NodeID, Name: req.Name, Kind: meta.KindDevice, TSStableID: req.TSStableID, Addr: req.Addr, RaftPort: req.RaftPort, RPCPort: req.RPCPort, Voter: req.Voter, Capacity: req.Capacity}}})
	if res.Err != nil {
		return JoinResponse{}, res.Err
	}
	return JoinResponse{NodeIdx: meta.NodeIdx(res.Ino), Volume: j.app.fsm.Volume(), Members: j.app.fsm.Nodes()}, nil
}

func (j *fakeJoiner) MemberOp(_ context.Context, op MemberOp) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.app.IsLeader() {
		return ErrNotLeader
	}
	j.ops = append(j.ops, op)
	return nil
}

type env struct {
	t      *testing.T
	app    *fakeApplier
	store  *chunk.Store
	ts     *tailscale.Fake
	auth   *WhoIsAuthorizer
	srv    *Server
	hs     *httptest.Server
	clk    *clock.Fake
	fence  fakeFence
	joiner *fakeJoiner
	bad    []BadSlot
	addr   string
	cid    [16]byte
}

var (
	selfIP   = netip.MustParseAddr("127.0.0.1") // httptest clients come from loopback
	memberIP = selfIP
)

// newEnv serves a node whose member table contains the loopback caller as member 1.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, app: newApplier(), clk: clock.NewFake(time.Now()), fence: fakeFence{placed: map[chunk.ShardKey]bool{}}}
	e.cid = [16]byte{1, 2, 3}
	var err error
	e.store, err = chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "shards"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	e.ts = tailscale.NewFake(memberIP, "self-1", nil)
	e.app.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "n1", Name: "one", Kind: meta.KindDevice, TSStableID: "self-1", Addr: memberIP.String(), Capacity: 1 << 30}}})
	e.auth = &WhoIsAuthorizer{TS: e.ts, Members: e.app.fsm.Nodes, Clk: e.clk}
	e.joiner = &fakeJoiner{app: e.app}
	e.srv, err = NewServer(Options{NodeID: "n1", ClusterID: e.cid, Version: "t", Store: e.store, Applier: e.app, Fence: e.fence, Joiner: e.joiner,
		Auth: e.auth, Members: e.app.fsm.Nodes, JoinToken: func() string { return "tok" }, Clk: e.clk, MaxShard: 1 << 16, Grace: 10 * time.Minute,
		BadSlot: func(k chunk.ShardKey, n meta.NodeIdx) { e.bad = append(e.bad, BadSlot{Key: string(k), Node: n}) }})
	if err != nil {
		t.Fatal(err)
	}
	e.hs = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.hs.Close)
	e.addr = e.hs.Listener.Addr().String()
	return e
}

func (e *env) client() *Client {
	return NewClient(e.cid, func(idx meta.NodeIdx) (string, bool, bool) {
		if idx == 1 {
			return e.addr, false, true // route "self" over HTTP so the server path is exercised
		}
		return "", false, false
	}, nil)
}

func key(payload []byte, idx int) chunk.ShardKey { return chunk.Key(sha256.Sum256(payload), 2, 1, idx) }

// Invariant: I-11
// Lifecycle: L-39, L-45, L-49
func TestChunkRPC_PutGetHeadDelete_SizeBoundaries(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for _, n := range []int{0, 1, (1 << 16) - 1, 1 << 16} {
		p := bytes.Repeat([]byte{byte(n)}, n)
		k := key(p, 0)
		if err := c.Put(ctx, 1, k, p); err != nil {
			t.Fatalf("put %d: %v", n, err)
		}
		if ok, err := c.Has(ctx, 1, k); err != nil || !ok {
			t.Fatalf("has %d: %v %v", n, ok, err)
		}
		got, err := c.Get(ctx, 1, k)
		if err != nil || !bytes.Equal(got, p) {
			t.Fatalf("get %d: %v (len %d)", n, err, len(got))
		}
		// duplicate PUT is idempotent
		if err := c.Put(ctx, 1, k, p); err != nil {
			t.Fatalf("dup put %d: %v", n, err)
		}
	}
	big := bytes.Repeat([]byte{7}, (1<<16)+1)
	if err := c.Put(ctx, 1, key(big, 0), big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	if _, err := c.Get(ctx, 1, key([]byte("missing"), 0)); !errors.Is(err, chunk.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if ok, _ := c.Has(ctx, 1, key([]byte("missing"), 0)); ok {
		t.Fatal("has missing")
	}
}

// Lifecycle: L-45
func TestChunkRPC_HashMismatchRejected(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	p := []byte("payload")
	k := key(p, 0)
	resp, err := c.do(ctx, http.MethodPut, e.addr, "/v1/shards/"+string(k), p, map[string]string{HeaderShardSum: "00ff"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e.store.Has(k) {
		t.Fatal("mismatched shard stored")
	}
	// bad key is 400 too
	resp, _ = c.do(ctx, http.MethodGet, e.addr, "/v1/shards/not-a-key", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad key status %d", resp.StatusCode)
	}
}

// Lifecycle: L-44
func TestChunkRPC_FullNodeIs507(t *testing.T) {
	e := newEnv(t)
	small, _ := chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "s"), 100)
	e.srv.o.Store = small
	c := e.client()
	p := bytes.Repeat([]byte{1}, 200)
	if err := c.Put(ctx, 1, key(p, 0), p); !errors.Is(err, chunk.ErrFull) {
		t.Fatalf("full: %v", err)
	}
}

// Review #4/#14: the DELETE fence.
// Invariant: I-13, I-14
// Lifecycle: L-134, L-137, L-169
func TestChunkRPC_DeleteFence(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	p := []byte("to be collected")
	k := key(p, 0)
	must(t, c.Put(ctx, 1, k, p))
	// no header → 400
	resp, _ := c.do(ctx, http.MethodDelete, e.addr, "/v1/shards/"+string(k), nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no index: %d", resp.StatusCode)
	}
	// index not yet applied here → 412
	if err := c.DeleteFenced(ctx, 1, k, e.app.AppliedIndex()+5); !errors.Is(err, ErrFenced) {
		t.Fatalf("future index: %v", err)
	}
	// shard younger than grace → 412
	if err := c.DeleteFenced(ctx, 1, k, e.app.AppliedIndex()); !errors.Is(err, ErrFenced) {
		t.Fatalf("young: %v", err)
	}
	e.clk.Advance(11 * time.Minute)
	// placed on self → 412 even when old
	e.fence.placed[k] = true
	if err := c.DeleteFenced(ctx, 1, k, e.app.AppliedIndex()); !errors.Is(err, ErrFenced) {
		t.Fatalf("placed: %v", err)
	}
	delete(e.fence.placed, k)
	if err := c.DeleteFenced(ctx, 1, k, e.app.AppliedIndex()); err != nil {
		t.Fatalf("valid fence: %v", err)
	}
	if e.store.Has(k) {
		t.Fatal("shard survived a valid fenced delete")
	}
	// deleting a missing shard is fine (idempotent)
	if err := c.DeleteFenced(ctx, 1, k, e.app.AppliedIndex()); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
}

// Invariant: I-18
func TestWhoIs_DeniesUnknownPeer_AndStableIDMismatch_AndPurge(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	// member whose tailnet identity changed (IP reassigned): WhoIs says another StableID
	e.ts.Remove(memberIP)
	e.ts.Add(memberIP, "stranger", "x")
	e.auth.Purge()
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stable id mismatch: %v", err)
	}
	// restore identity: a mismatch purged the cache, so the next call re-resolves
	e.ts.Remove(memberIP)
	e.ts.Add(memberIP, "self-1", "self")
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, chunk.ErrNotFound) {
		t.Fatalf("after restore: %v", err)
	}
	// not on the tailnet at all → forbidden, and negative-cached
	e.ts.Remove(memberIP)
	e.auth.Purge()
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("off tailnet: %v", err)
	}
	e.ts.Add(memberIP, "self-1", "self")
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, ErrForbidden) {
		t.Fatal("negative cache not honoured")
	}
	e.clk.Advance(time.Minute)
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, chunk.ErrNotFound) {
		t.Fatalf("after negative TTL: %v", err)
	}
	// removed member → forbidden regardless of WhoIs
	e.app.apply(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: 1, State: meta.Removed, Now: 2}})
	e.auth.Purge()
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed member: %v", err)
	}
}

// Invariant: I-18
func TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader(t *testing.T) {
	e := newEnv(t)
	e.ts.Remove(memberIP)
	e.auth.Purge()
	// a huge body with a forbidden source: the server must answer without reading it
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()
	req, _ := http.NewRequest(http.MethodPut, e.hs.URL+"/v1/shards/"+string(key([]byte("x"), 0)), pr)
	req.Header.Set(HeaderCluster, e.srv.clusID)
	req.ContentLength = 1 << 40
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- nil
			return
		}
		done <- resp
	}()
	select {
	case resp := <-done:
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("resp %v", resp)
		}
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("server waited for the body before authorizing")
	}
	// wrong cluster id → 409 before auth
	other := NewClient([16]byte{9}, func(meta.NodeIdx) (string, bool, bool) { return e.addr, false, true }, nil)
	if _, err := other.Get(ctx, 1, key([]byte("x"), 0)); !errors.Is(err, ErrWrongCluster) {
		t.Fatalf("cluster mismatch: %v", err)
	}
}

// Invariant: I-29
// Lifecycle: L-57
func TestApply_ForwardAndRedirect(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	cmd := meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "d", Mode: 0o755, Now: 5}}
	res, index, err := c.ApplyAt(ctx, e.addr, cmd)
	if err != nil || res.Err != nil || index == 0 || res.Ino == 0 {
		t.Fatalf("apply: %v %v %d", err, res.Err, index)
	}
	// FSM rejection travels in Result.Err
	res, _, err = c.ApplyAt(ctx, e.addr, cmd)
	if err != nil || !errors.Is(res.Err, meta.ErrExist) {
		t.Fatalf("dup: %v %v", err, res.Err)
	}
	// follower redirects to the leader; no leader is 503
	e.app.setLeader(false, "100.64.0.9:20481")
	_, _, err = c.ApplyAt(ctx, e.addr, cmd)
	if !errors.Is(err, ErrNotLeader) || LeaderFromErr(err) != "100.64.0.9:20481" {
		t.Fatalf("redirect: %v (%q)", err, LeaderFromErr(err))
	}
	e.app.setLeader(false, "")
	if _, _, err = c.ApplyAt(ctx, e.addr, cmd); !errors.Is(err, ErrNoLeader) {
		t.Fatalf("no leader: %v", err)
	}
	// garbage is 400
	resp, _ := c.do(ctx, http.MethodPost, e.addr, "/v1/apply", []byte("nope"), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage: %d", resp.StatusCode)
	}
}

// Invariant: I-48
// Lifecycle: L-96
func TestJoin_TokenAndIdentity(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	req := JoinRequest{Token: "wrong", NodeID: "n2", Name: "two", TSStableID: "self-1", RaftPort: 1, RPCPort: 2, Capacity: 5}
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wrong token: %v", err)
	}
	if len(e.app.fsm.Nodes()) != 1 {
		t.Fatal("wrong token changed membership")
	}
	req.Token, req.TSStableID = "tok", "someone-else"
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("identity mismatch: %v", err)
	}
	req.TSStableID = "self-1"
	jr, err := c.Join(ctx, e.addr, req)
	if err != nil || jr.NodeIdx != 2 || len(jr.Members) != 2 || jr.Volume.K != 2 {
		t.Fatalf("join: %+v %v", jr, err)
	}
	if n := e.app.fsm.Nodes()[1]; n.Addr != memberIP.String() || n.ID != "n2" || n.State != meta.Active {
		t.Fatalf("joined node %+v", n)
	}
	// join through a follower is redirected
	e.app.setLeader(false, "100.64.0.9:20481")
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower join: %v", err)
	}
}

func TestMembersBadSlotHealthStatus(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	must(t, c.Members(ctx, e.addr, MemberOp{Op: "set_capacity", NodeIdx: 1, Capacity: 7}))
	if len(e.joiner.ops) != 1 || e.joiner.ops[0].Capacity != 7 {
		t.Fatalf("ops %+v", e.joiner.ops)
	}
	k := key([]byte("bad"), 1)
	must(t, c.ReportBadSlot(ctx, e.addr, k, 3))
	if len(e.bad) != 1 || e.bad[0].Key != string(k) || e.bad[0].Node != 3 {
		t.Fatalf("bad slots %+v", e.bad)
	}
	h, err := c.Health(ctx, e.addr)
	if err != nil || h.NodeID != "n1" || !h.Leader || h.Capacity != 1<<30 || h.Applied != e.app.AppliedIndex() {
		t.Fatalf("health %+v %v", h, err)
	}
	st, err := c.Status(ctx, e.addr)
	if err != nil || !bytes.Contains(st, []byte(`"members"`)) {
		t.Fatalf("status %s %v", st, err)
	}
	// health needs no cluster header and no membership
	resp, err := http.Get(e.hs.URL + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("plain health: %v", err)
	}
	resp.Body.Close()
}

func TestListen_RefusesNonLoopbackWithoutAuthorizer(t *testing.T) {
	if _, err := Listen("0.0.0.0:0", AllowAll{}); err == nil {
		t.Fatal("bound without authorizer")
	}
	l, err := Listen("127.0.0.1:0", AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	_ = strconv.Itoa
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
