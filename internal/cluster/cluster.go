// Package cluster wires hashicorp/raft around the metadata FSM (docs/02). M1 ships the
// single-node path: bootstrap, open, propose, durable restart. M2 adds membership,
// forwarding and liveness.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// Errors.
var (
	ErrNotLeader    = errors.New("cluster: not the leader")
	ErrNoLeader     = errors.New("cluster: no leader")
	ErrAlreadyInit  = errors.New("cluster: data directory already holds raft state")
	ErrNotInit      = errors.New("cluster: data directory is not initialised (run init or join)")
	ErrAlreadyOpen  = errors.New("cluster: data directory is in use by another process")
	ErrUnknownReply = errors.New("cluster: unexpected FSM reply")
)

// Options configure Open and Bootstrap.
type Options struct {
	DataDir   string
	NodeID    string         // raft.ServerID; LoadOrCreateNodeID
	RaftAddr  string         // TCP bind/advertise address when Transport is nil
	Transport raft.Transport // tests pass raft.NewInmemTransport
	Logger    *slog.Logger
}

// Node is an open Raft member.
type Node struct {
	ID   string
	Raft *raft.Raft
	FSM  *meta.FSM

	logs   *raftboltdb.BoltStore
	trans  raft.Transport
	log    *slog.Logger
	closed bool
}

// RaftConfig returns the documented tuning (docs/02 §Raft configuration).
func RaftConfig(id string, logOutput io.Writer) *raft.Config {
	c := raft.DefaultConfig()
	c.LocalID = raft.ServerID(id)
	c.HeartbeatTimeout = 2 * time.Second
	c.ElectionTimeout = 2 * time.Second
	c.LeaderLeaseTimeout = time.Second
	c.CommitTimeout = 50 * time.Millisecond
	c.SnapshotInterval = 2 * time.Minute
	c.SnapshotThreshold = 8192
	c.TrailingLogs = 10240
	c.LogOutput = logOutput
	return c
}

// LoadOrCreateNodeID returns the persisted node ID, creating a random one on first use.
func LoadOrCreateNodeID(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "node_id")
	if b, err := os.ReadFile(p); err == nil {
		id := strings.TrimSpace(string(b))
		if len(id) == 16 {
			return id, nil
		}
		return "", fmt.Errorf("cluster: malformed %s", p)
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// HasState reports whether the data directory holds Raft state.
func HasState(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "raft", "raft.db"))
	return err == nil
}

type stores struct {
	logs  *raftboltdb.BoltStore
	snaps *raft.FileSnapshotStore
}

func openStores(dataDir string, logOutput io.Writer) (*stores, error) {
	dir := filepath.Join(dataDir, "raft")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	logs, err := raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(dir, "raft.db"),
		BoltOptions: &bbolt.Options{Timeout: time.Second},
		NoSync:      false, // every append is fsynced (docs/04 §8.1)
	})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, ErrAlreadyOpen
		}
		return nil, fmt.Errorf("cluster: open raft.db: %w", err)
	}
	snaps, err := raft.NewFileSnapshotStore(dir, 2, logOutput)
	if err != nil {
		_ = logs.Close()
		return nil, fmt.Errorf("cluster: snapshot store: %w", err)
	}
	return &stores{logs: logs, snaps: snaps}, nil
}

func (o *Options) transport(logOutput io.Writer) (raft.Transport, error) {
	if o.Transport != nil {
		return o.Transport, nil
	}
	addr, err := net.ResolveTCPAddr("tcp", o.RaftAddr)
	if err != nil {
		return nil, fmt.Errorf("cluster: raft address: %w", err)
	}
	return raft.NewTCPTransport(o.RaftAddr, addr, 3, 10*time.Second, logOutput)
}

func (o *Options) logger() (*slog.Logger, io.Writer) {
	l := o.Logger
	if l == nil {
		l = slog.Default()
	}
	return l, slogWriter{l}
}

type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.l.Debug(strings.TrimSpace(string(p)), "src", "raft")
	return len(p), nil
}

// Bootstrap creates a new single-voter cluster in an empty data directory and applies
// the initial metadata: cluster ID, join token, this node's record and the volume policy.
func Bootstrap(ctx context.Context, o Options, self meta.Node, vol meta.SetVolume, joinToken string) (*Node, error) {
	if HasState(o.DataDir) {
		return nil, ErrAlreadyInit
	}
	_, logOutput := o.logger()
	st, err := openStores(o.DataDir, logOutput)
	if err != nil {
		return nil, err
	}
	trans, err := o.transport(logOutput)
	if err != nil {
		_ = st.logs.Close()
		return nil, err
	}
	conf := RaftConfig(o.NodeID, logOutput)
	cfg := raft.Configuration{Servers: []raft.Server{{Suffrage: raft.Voter, ID: raft.ServerID(o.NodeID), Address: trans.LocalAddr()}}}
	if err := raft.BootstrapCluster(conf, st.logs, st.logs, st.snaps, trans, cfg); err != nil {
		_ = st.logs.Close()
		return nil, fmt.Errorf("cluster: bootstrap: %w", err)
	}
	n, err := start(o, st, trans)
	if err != nil {
		return nil, err
	}
	if err := n.WaitReady(ctx, 10*time.Second); err != nil {
		_ = n.Close()
		return nil, err
	}
	var cid [16]byte
	_, _ = rand.Read(cid[:])
	self.Idx = 0 // allocate
	self.ID = o.NodeID
	if self.Kind == 0 {
		self.Kind = meta.KindDevice
	}
	cmds := []meta.Command{
		{Op: meta.OpMetaSet, MetaSet: &meta.MetaSet{ClusterID: cid, JoinToken: joinToken, CreatedAt: time.Now().UnixNano()}},
		{Op: meta.OpSetVolume, SetVolume: &vol},
		{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: self}},
	}
	for _, c := range cmds {
		res, _, err := n.Propose(ctx, c)
		if err == nil {
			err = res.Err
		}
		if err != nil {
			_ = n.Close()
			return nil, fmt.Errorf("cluster: bootstrap op %d: %w", c.Op, err)
		}
	}
	if err := os.WriteFile(filepath.Join(o.DataDir, "cluster_id"), []byte(hex.EncodeToString(cid[:])+"\n"), 0o600); err != nil {
		_ = n.Close()
		return nil, err
	}
	return n, nil
}

// Open starts Raft on an initialised data directory.
func Open(o Options) (*Node, error) {
	if !HasState(o.DataDir) {
		return nil, ErrNotInit
	}
	_, logOutput := o.logger()
	st, err := openStores(o.DataDir, logOutput)
	if err != nil {
		return nil, err
	}
	trans, err := o.transport(logOutput)
	if err != nil {
		_ = st.logs.Close()
		return nil, err
	}
	return start(o, st, trans)
}

func start(o Options, st *stores, trans raft.Transport) (*Node, error) {
	log, logOutput := o.logger()
	fsm := meta.NewFSM()
	r, err := raft.NewRaft(RaftConfig(o.NodeID, logOutput), fsm, st.logs, st.logs, st.snaps, trans)
	if err != nil {
		_ = st.logs.Close()
		return nil, fmt.Errorf("cluster: start raft: %w", err)
	}
	return &Node{ID: o.NodeID, Raft: r, FSM: fsm, logs: st.logs, trans: trans, log: log}, nil
}

// WaitLeader blocks until this node sees a leader or the timeout passes.
func (n *Node) WaitLeader(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if addr, _ := n.Raft.LeaderWithID(); addr != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return ErrNoLeader
		case <-tick.C:
		}
	}
}

// WaitReady waits for a leader and for this node's FSM to have applied every log
// entry Raft already holds, so reads of the node table and volume policy see the state
// as of the last commit (a freshly elected leader replays its log asynchronously).
func (n *Node) WaitReady(ctx context.Context, timeout time.Duration) error {
	if err := n.WaitLeader(ctx, timeout); err != nil {
		return err
	}
	// Raft's AppliedIndex advances from the FSM goroutine after each apply and counts
	// configuration entries the FSM never sees, so it is the right catch-up marker.
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for n.Raft.AppliedIndex() < n.Raft.LastIndex() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("cluster: log replay incomplete after %s (applied %d of %d)", timeout, n.Raft.AppliedIndex(), n.Raft.LastIndex())
		case <-tick.C:
		}
	}
	return nil
}

// IsLeader reports whether this node currently leads.
func (n *Node) IsLeader() bool { return n.Raft.State() == raft.Leader }

// Propose implements meta.Proposer by applying through Raft. M1: leader only; a
// follower returns ErrNotLeader (forwarding arrives with M2).
func (n *Node) Propose(ctx context.Context, cmd meta.Command) (meta.Result, uint64, error) {
	if !n.IsLeader() {
		if addr, _ := n.Raft.LeaderWithID(); addr == "" {
			return meta.Result{}, 0, ErrNoLeader
		}
		return meta.Result{}, 0, ErrNotLeader
	}
	timeout := 5 * time.Second
	if d, ok := ctx.Deadline(); ok {
		timeout = time.Until(d)
	}
	fut := n.Raft.Apply(meta.EncodeCommand(cmd), timeout)
	if err := fut.Error(); err != nil {
		return meta.Result{}, 0, fmt.Errorf("cluster: apply: %w", err)
	}
	res, ok := fut.Response().(meta.Result)
	if !ok {
		return meta.Result{}, 0, ErrUnknownReply
	}
	return res, fut.Index(), nil
}

// Close shuts Raft down and releases the stores.
func (n *Node) Close() error {
	if n.closed {
		return nil
	}
	n.closed = true
	var first error
	if err := n.Raft.Shutdown().Error(); err != nil {
		first = err
	}
	if c, ok := n.trans.(io.Closer); ok {
		_ = c.Close()
	}
	if err := n.logs.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// --- single-node views used by M1 ---------------------------------------------------

// LocalPeers is the liveness view of a node that is alone in its cluster.
type LocalPeers struct{ Idx meta.NodeIdx }

func (p LocalPeers) Self() meta.NodeIdx                       { return p.Idx }
func (p LocalPeers) Live() []meta.NodeIdx                     { return []meta.NodeIdx{p.Idx} }
func (p LocalPeers) Rank(nodes []meta.NodeIdx) []meta.NodeIdx { return nodes }

// LocalShards routes shard traffic for this node to its own store; other nodes are
// unreachable until the M2 RPC client takes over.
type LocalShards struct {
	Idx   meta.NodeIdx
	Store *chunk.Store
}

var errRemoteUnavailable = errors.New("cluster: remote shard transport not available")

func (s LocalShards) Put(_ context.Context, node meta.NodeIdx, key chunk.ShardKey, body []byte) error {
	if node != s.Idx {
		return errRemoteUnavailable
	}
	_, err := s.Store.Put(key, body)
	return err
}

func (s LocalShards) Get(_ context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	if node != s.Idx {
		return nil, errRemoteUnavailable
	}
	return s.Store.Get(key)
}

func (s LocalShards) Delete(_ context.Context, node meta.NodeIdx, key chunk.ShardKey) error {
	if node != s.Idx {
		return errRemoteUnavailable
	}
	_, err := s.Store.Delete(key)
	return err
}

// SelfIdx finds this node's NodeIdx in the FSM by raft ID.
func (n *Node) SelfIdx() (meta.NodeIdx, bool) {
	nodes := n.FSM.Nodes()
	for i := range nodes {
		if nodes[i].ID == n.ID {
			return nodes[i].Idx, true
		}
	}
	return 0, false
}
