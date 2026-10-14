package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
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
	proposeErr error // returned by Propose after applying: the leader lost the outcome
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
	res := a.fsm.Apply(&raft.Log{Index: a.idx, Type: raft.LogCommand, Data: meta.MustEncodeCommand(c)}).(meta.Result)
	return res, a.idx
}

func (a *fakeApplier) Propose(_ context.Context, c meta.Command) (meta.Result, uint64, error) {
	if !a.IsLeader() {
		return meta.Result{}, 0, ErrNotLeader
	}
	r, i := a.apply(c)
	a.mu.Lock()
	perr := a.proposeErr
	a.mu.Unlock()
	if perr != nil {
		return meta.Result{}, 0, perr
	}
	return r, i, nil
}
func (a *fakeApplier) IsLeader() bool        { a.mu.Lock(); defer a.mu.Unlock(); return a.leader }
func (a *fakeApplier) LeaderRPCAddr() string { a.mu.Lock(); defer a.mu.Unlock(); return a.leaderAddr }
func (a *fakeApplier) AppliedIndex() uint64  { return a.fsm.Applied() }
func (a *fakeApplier) Ready() bool           { return a.IsLeader() || a.LeaderRPCAddr() != "" }
func (a *fakeApplier) setLeader(l bool, addr string) {
	a.mu.Lock()
	a.leader, a.leaderAddr = l, addr
	a.mu.Unlock()
}

type fakeFence struct{ placed map[chunk.ShardKey]bool }

func (f fakeFence) PlacedOn(_ meta.NodeIdx, k chunk.ShardKey) bool { return f.placed[k] }

type fakeJoiner struct {
	mu   sync.Mutex
	app  *fakeApplier
	ops  []MemberOp
	fail error // when set, every join fails with it (a leader that lost its lease mid-proposal)
}

func (j *fakeJoiner) Join(_ context.Context, req JoinRequest) (JoinResponse, error) {
	if !j.app.IsLeader() {
		return JoinResponse{}, ErrNotLeader
	}
	if j.fail != nil {
		return JoinResponse{}, j.fail
	}
	var key meta.MeshKey
	if req.Key != "" {
		b, err := base64.StdEncoding.DecodeString(req.Key)
		if err != nil || len(b) != len(key) {
			return JoinResponse{}, errors.New("bad key")
		}
		copy(key[:], b)
	}
	res, _ := j.app.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{
		ID: req.NodeID, Name: req.Name, Kind: meta.KindDevice, TSStableID: req.TSStableID, Addr: req.Addr, RaftPort: req.RaftPort, RPCPort: req.RPCPort, Voter: req.Voter, Key: key,
	}}})
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
	t       *testing.T
	app     *fakeApplier
	store   *chunk.Store
	auth    *TableAuthorizer
	deny    atomic.Bool // the caller is a stranger
	pending atomic.Bool // the caller is a joiner in progress
	srv     *Server
	hs      *httptest.Server
	clk     *clock.Fake
	fence   fakeFence
	joiner  *fakeJoiner
	bad     []BadSlot
	addr    string
	cid     [16]byte
}

var (
	selfIP   = netip.MustParseAddr("127.0.0.1") // httptest clients come from loopback
	memberIP = selfIP
)

// newEnv serves a node whose member table contains the loopback caller as member 1.
func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith builds the environment with mod applied to the server options first.
func newEnvWith(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	e := &env{t: t, app: newApplier(), clk: clock.NewFake(time.Now()), fence: fakeFence{placed: map[chunk.ShardKey]bool{}}}
	e.cid = [16]byte{1, 2, 3}
	var err error
	e.store, err = chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "shards"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	e.app.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "n1", Name: "one", Kind: meta.KindDevice, TSStableID: "self-1", Addr: memberIP.String(), Capacity: 1 << 30}}})
	// the table authorizer over the fake FSM; deny makes the caller a stranger (a row the
	// table does not hold) and pending a joiner in progress (docs/20 §4)
	e.auth = &TableAuthorizer{
		Member: func(addr string) (meta.Node, bool) {
			if e.deny.Load() {
				return meta.Node{}, false
			}
			return e.app.fsm.NodeByAddr(addr)
		},
		Pending: func(addr string) bool { return e.pending.Load() && addr == memberIP.String() },
	}
	e.joiner = &fakeJoiner{app: e.app}
	opts := Options{
		NodeID: "n1", ClusterID: e.cid, Version: "t", Applier: e.app, Fence: e.fence, Joiner: e.joiner, Disk: e.diskLookup, DiskHealth: e.diskHealth,
		Auth: e.auth, Members: e.app.fsm.Nodes, JoinToken: func() string { return "tok" }, KeylessJoin: true, Clk: e.clk, MaxShard: 1 << 16, Grace: 10 * time.Minute,
		BadSlot: func(k chunk.ShardKey, n meta.NodeIdx) { e.bad = append(e.bad, BadSlot{Key: string(k), Node: n}) },
	}
	if mod != nil {
		mod(&opts)
	}
	e.srv, err = NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	e.hs = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.hs.Close)
	e.addr = e.hs.Listener.Addr().String()
	return e
}

// The server's one disk, "d0", is target 1 (a device stores nothing, docs/14 §9: the
// rpc package only routes by disk ID).
func (e *env) diskLookup(id string) (ShardStore, meta.NodeIdx, bool) {
	if id == "d0" {
		return e.store, 1, true
	}
	return nil, 0, false
}

func (e *env) diskHealth() []DiskHealth {
	return []DiskHealth{{ID: "d0", Used: e.store.Used(), Capacity: e.store.Capacity(), Free: e.store.Capacity() - e.store.Used(), OK: true}}
}

// disk0 names target 1 as disk "d0" for a client.
func disk0(idx meta.NodeIdx) (string, bool) {
	if idx == 1 {
		return "d0", true
	}
	return "", false
}

func (e *env) client() *Client {
	c := NewClient(e.cid, func(idx meta.NodeIdx) (string, bool, bool) {
		if idx == 1 {
			return e.addr, false, true // route "self" over HTTP so the server path is exercised
		}
		return "", false, false
	})
	c.SetDisks(disk0, nil)
	return c
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
	resp, err := c.do(ctx, http.MethodPut, e.addr, "/v1/disks/d0/shards/"+string(k), p, map[string]string{HeaderShardSum: "00ff"})
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
	resp, _ = c.do(ctx, http.MethodGet, e.addr, "/v1/disks/d0/shards/not-a-key", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad key status %d", resp.StatusCode)
	}
}

// Lifecycle: L-44
func TestChunkRPC_FullNodeIs507(t *testing.T) {
	e := newEnv(t)
	small, _ := chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "s"), 100)
	e.store = small
	c := e.client()
	p := bytes.Repeat([]byte{1}, 200)
	if err := c.Put(ctx, 1, key(p, 0), p); !errors.Is(err, chunk.ErrFull) {
		t.Fatalf("full: %v", err)
	}
}

// Review #4/#14: the DELETE fence.
// Invariant: I-13, I-14
// Lifecycle: L-134, L-137, L-169, L-174
func TestChunkRPC_DeleteFence(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	p := []byte("to be collected")
	k := key(p, 0)
	must(t, c.Put(ctx, 1, k, p))
	// no header → 400
	resp, _ := c.do(ctx, http.MethodDelete, e.addr, "/v1/disks/d0/shards/"+string(k), nil, nil)
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

// Invariant: I-18, I-318
func TestTable_DeniesStrangerPendingAndRemoved(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, chunk.ErrNotFound) {
		t.Fatalf("member: %v", err)
	}
	// an address the table does not hold: a stranger, refused before any body
	e.deny.Store(true)
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger: %v", err)
	}
	// a joiner in progress may call /health and /v1/join and nothing else (docs/20 §4)
	e.pending.Store(true)
	if _, err := c.Health(ctx, e.addr); err != nil {
		t.Fatalf("pending health: %v", err)
	}
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("pending shard read: %v", err)
	}
	e.deny.Store(false)
	e.pending.Store(false)
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, chunk.ErrNotFound) {
		t.Fatalf("member again: %v", err)
	}
	// a removed row is no member, with nothing cached to purge
	e.app.apply(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: 1, State: meta.Removed, Now: 2}})
	if _, err := c.Get(ctx, 1, key([]byte("p"), 0)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed member: %v", err)
	}
}

// Invariant: I-18, I-49
func TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader(t *testing.T) {
	e := newEnv(t)
	e.deny.Store(true)
	// a huge body with a forbidden source: the server must answer without reading it
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()
	req, _ := http.NewRequest(http.MethodPut, e.hs.URL+"/v1/disks/d0/shards/"+string(key([]byte("x"), 0)), pr)
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
	other := NewClient([16]byte{9}, func(meta.NodeIdx) (string, bool, bool) { return e.addr, false, true })
	other.SetDisks(disk0, nil)
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

// After a failed attempt a source IP gets one join attempt per second, so the 8-character
// token cannot be brute-forced from inside the tailnet (docs/08 threat table); successful
// joins are never throttled.
// Invariant: I-48
func TestJoin_RateLimitedPerSource(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	req := JoinRequest{Token: "wrong", NodeID: "n2", Name: "two", TSStableID: "self-1", RaftPort: 1, RPCPort: 2}
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("first attempt: %v", err)
	}
	req.Token = "tok"
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second attempt within a second: err=%v, want ErrRateLimited", err)
	}
	if len(e.app.fsm.Nodes()) != 1 {
		t.Fatal("a rate-limited join changed membership")
	}
	e.clk.Advance(JoinInterval)
	if _, err := c.Join(ctx, e.addr, req); err != nil {
		t.Fatalf("attempt after the interval: %v", err)
	}
}

// Invariant: I-48, I-321
// Lifecycle: L-96
func TestJoin_TokenAndKey(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	req := JoinRequest{Token: "wrong", NodeID: "n2", Name: "two", RaftPort: 1, RPCPort: 2}
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wrong token: %v", err)
	}
	if len(e.app.fsm.Nodes()) != 1 {
		t.Fatal("wrong token changed membership")
	}
	e.clk.Advance(JoinInterval) // a failed attempt throttles the source for a second
	// a mesh join needs an introduce: without a pending row the key is refused, token or not
	keyed := req
	keyed.Token, keyed.Key = "tok", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if _, err := c.Join(ctx, e.addr, keyed); !errors.Is(err, ErrForbidden) {
		t.Fatalf("keyed join without an introduce: %v", err)
	}
	e.pending.Store(true)
	jr, err := c.Join(ctx, e.addr, keyed)
	if err != nil || jr.NodeIdx != 2 || len(jr.Members) != 2 {
		t.Fatalf("keyed join from a pending address: %+v %v", jr, err)
	}
	if n := e.app.fsm.Nodes()[1]; n.Addr != memberIP.String() || n.ID != "n2" {
		t.Fatalf("joined node %+v", n)
	}
	e.pending.Store(false)
	// a keyless join with the token, where the server allows one (the harness, the fake)
	req.Token, req.NodeID = "tok", "n3"
	jr, err = c.Join(ctx, e.addr, req)
	if err != nil || jr.NodeIdx != 3 || jr.Volume.K != 2 {
		t.Fatalf("keyless join: %+v %v", jr, err)
	}
	// join through a follower is redirected
	e.app.setLeader(false, "100.64.0.9:20481")
	if _, err := c.Join(ctx, e.addr, req); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower join: %v", err)
	}
	// a node on the mesh refuses a keyless join: it joins by code
	strict := newEnvWith(t, func(o *Options) { o.KeylessJoin = false })
	if _, err := strict.client().Join(ctx, strict.addr, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("keyless join on the mesh: %v", err)
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
	if err != nil || h.NodeID != "n1" || !h.Leader || len(h.Disks) != 1 || h.Disks[0].Capacity != 1<<30 || h.Applied != e.app.AppliedIndex() || h.Codec != meta.CodecVersion {
		t.Fatalf("health %+v %v", h, err)
	}
	st, err := c.Call(ctx, e.addr, http.MethodGet, "/v1/status", nil)
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

// Invariant: I-50
func TestListen_RefusesNonLoopbackWithoutAuthorizer(t *testing.T) {
	if _, err := Listen(t.Context(), "0.0.0.0:0", AllowAll{}); err == nil {
		t.Fatal("bound without authorizer")
	}
	l, err := Listen(t.Context(), "127.0.0.1:0", AllowAll{})
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

// A join whose row proposal had no known outcome (the leader lost its lease with the
// new voter not yet caught up) answers as retryable, not as a bad request: the joiner
// tries again under the same node ID, which keeps its row and index whichever way the
// proposal went.
//
// Invariant: I-94, I-95
func TestJoin_UnknownOutcomeIsRetryable(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	req := JoinRequest{Token: "tok", NodeID: "n2", Name: "two", TSStableID: "self-1", RaftPort: 1, RPCPort: 2}
	e.joiner.fail = fmt.Errorf("cluster: record member: %w: apply: leadership lost while committing log", meta.ErrUnknownOutcome)
	_, err := c.Join(ctx, e.addr, req)
	if !errors.Is(err, ErrNoLeader) || !strings.Contains(err.Error(), "outcome unknown") {
		t.Fatalf("unknown outcome: %v, want a retryable no-leader error naming it", err)
	}
	e.clk.Advance(JoinInterval)
	e.joiner.fail = fmt.Errorf("cluster: record member: %w", meta.ErrNotDir)
	if _, err := c.Join(ctx, e.addr, req); errors.Is(err, ErrNoLeader) || err == nil {
		t.Fatalf("a known failure stays a bad request: %v", err)
	}
	e.clk.Advance(JoinInterval)
	e.joiner.fail = nil
	if jr, err := c.Join(ctx, e.addr, req); err != nil || jr.NodeIdx != 2 {
		t.Fatalf("the retry: %+v %v", jr, err)
	}
}

// GET /v1/stats is a member route that answers this node's own unsealed rows newer than
// since, and an empty list without a sampler; /health carries the web port.
// Invariant: I-199
func TestRPC_Stats_MemberOnly_OwnRows(t *testing.T) {
	var gotSince int64
	e := newEnvWith(t, func(o *Options) {
		o.Web = 20482
		o.Stats = func(since int64) any {
			gotSince = since
			return []map[string]any{{"t": since + 1, "n": "one"}, {"t": since + 2, "n": "one"}}
		}
	})
	c := e.client()
	body, err := c.Call(ctx, e.addr, http.MethodGet, "/v1/stats?since=5", nil)
	if err != nil || gotSince != 5 || !bytes.Contains(body, []byte(`"t":6`)) || !bytes.Contains(body, []byte(`"t":7`)) {
		t.Fatalf("stats: %s %v (since %d)", body, err, gotSince)
	}
	if body, err := c.Call(ctx, e.addr, http.MethodGet, "/v1/stats", nil); err != nil || gotSince != 0 {
		t.Fatalf("stats without since: %s %v (since %d)", body, err, gotSince)
	}
	h, err := c.Health(ctx, e.addr)
	if err != nil || h.Web != 20482 {
		t.Fatalf("health web port %d %v", h.Web, err)
	}
	// a stranger (the WhoIs identity no longer matches the member record) is refused
	e.deny.Store(true)
	if _, err := c.Call(ctx, e.addr, http.MethodGet, "/v1/stats?since=5", nil); err == nil {
		t.Fatal("a non-member read the stats")
	}
	// without a sampler: an empty list, never an error
	e2 := newEnv(t)
	if body, err := e2.client().Call(ctx, e2.addr, http.MethodGet, "/v1/stats", nil); err != nil || bytes.TrimSpace(body) == nil || string(bytes.TrimSpace(body)) != "[]" {
		t.Fatalf("no sampler: %s %v", body, err)
	}
	if h, err := e2.client().Health(ctx, e2.addr); err != nil || h.Web != 0 {
		t.Fatalf("health without a web UI: %d %v", h.Web, err)
	}
}

// GET /v1/members answers a member the device rows of this node's table, without disks,
// removed rows or rows without an address, and refuses a non-member: a restarting node
// widens its seed with it (docs/08, I-282).
//
// Invariant: I-282
func TestMemberList_MembersOnly_DeviceRows(t *testing.T) {
	e := newEnvWith(t, func(o *Options) {
		o.Members = func() []meta.Node {
			return []meta.Node{
				{Idx: 1, ID: "a", Kind: meta.KindDevice, Addr: "100.64.0.11", TSStableID: "ts-a", RPCPort: 20481},
				{Idx: 2, ID: "a/1", Kind: meta.KindDisk, Host: 1},
				{Idx: 3, ID: "late", Kind: meta.KindDevice, Addr: "100.64.0.31", TSStableID: "ts-late", RPCPort: 20481},
				{Idx: 4, ID: "gone", Kind: meta.KindDevice, Addr: "100.64.0.9", State: meta.Removed},
				{Idx: 5, ID: "noaddr", Kind: meta.KindDevice},
			}
		}
	})
	c := e.client()
	got, err := c.MemberList(ctx, e.addr)
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "late" || got[1].TSStableID != "ts-late" {
		t.Fatalf("member list: %+v %v", got, err)
	}
	e.deny.Store(true)
	if got, err := c.MemberList(ctx, e.addr); err == nil {
		t.Fatalf("a non-member read the member list: %+v", got)
	}
}

// GET /v1/secrets/key answers a member with the key this node holds, 404 when it holds
// none, and nothing to a non-member or to a caller of another cluster (docs/19 §3).
//
// Invariant: I-304
func TestSecretKey_MembersOnly(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	held := true
	e := newEnvWith(t, func(o *Options) {
		o.SecretKey = func() ([]byte, bool) { return key, held }
	})
	c := e.client()
	if got, err := c.SecretKey(ctx, e.addr); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("a member's fetch: %q %v", got, err)
	}
	held = false
	if got, err := c.SecretKey(ctx, e.addr); err == nil {
		t.Fatalf("a node without the key answered %q", got)
	}
	held = true
	other := NewClient([16]byte{9}, nil)
	if got, err := other.SecretKey(ctx, e.addr); err == nil {
		t.Fatalf("another cluster read the key: %q", got)
	}
	e.deny.Store(true)
	if got, err := c.SecretKey(ctx, e.addr); err == nil {
		t.Fatalf("a non-member read the key: %q", got)
	}
}
