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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// Errors.
var (
	ErrNotLeader    = fmt.Errorf("cluster: not the leader: %w", rpc.ErrNotLeader)
	ErrNoLeader     = fmt.Errorf("cluster: no leader: %w", rpc.ErrNoLeader)
	ErrAlreadyInit  = errors.New("cluster: data directory already holds raft state")
	ErrNotInit      = errors.New("cluster: data directory is not initialised (run init or join)")
	ErrAlreadyOpen  = errors.New("cluster: data directory is in use by another process")
	ErrUnknownReply = errors.New("cluster: unexpected FSM reply")
)

// Options configure Open and Bootstrap.
type Options struct {
	DataDir   string
	NodeID    string         // raft.ServerID; LoadOrCreateNodeID
	ClusterID [16]byte       // for the Raft stream handshake; Bootstrap generates it
	RaftAddr  string         // TCP bind/advertise address when Transport is nil
	Transport raft.Transport // tests pass raft.NewInmemTransport
	Logger    *slog.Logger
	// Fast shortens the Raft timeouts to tens of milliseconds and lowers the snapshot
	// threshold; tests only.
	Fast bool
	// SnapshotThreshold overrides the documented 8192 (tests of snapshot install).
	SnapshotThreshold uint64
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

	fwdMu sync.Mutex
	fwd   Forwarder
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
	return NewTransport(o.RaftAddr, o.ClusterID, logOutput)
}

// LoadClusterID reads <data_dir>/cluster_id.
func LoadClusterID(dataDir string) ([16]byte, error) {
	var id [16]byte
	b, err := os.ReadFile(filepath.Join(dataDir, "cluster_id"))
	if err != nil {
		return id, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != 16 {
		return id, fmt.Errorf("cluster: malformed cluster_id")
	}
	copy(id[:], raw)
	return id, nil
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
	var cid [16]byte
	_, _ = rand.Read(cid[:])
	o.ClusterID = cid
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.DataDir, "cluster_id"), []byte(hex.EncodeToString(cid[:])+"\n"), 0o600); err != nil {
		return nil, err
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
	conf := o.raftConfig(logOutput)
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

func (o *Options) raftConfig(logOutput io.Writer) *raft.Config {
	c := RaftConfig(o.NodeID, logOutput)
	if o.Fast {
		// Short enough for quick elections in tests, long enough that a leader under the
		// race detector's load does not lose its lease to a slow follower (CI showed
		// 25 ms contact times stepping a 25 ms lease down into permanent churn).
		c.HeartbeatTimeout = 300 * time.Millisecond
		c.ElectionTimeout = 300 * time.Millisecond
		c.LeaderLeaseTimeout = 150 * time.Millisecond
		c.CommitTimeout = 10 * time.Millisecond
		c.SnapshotInterval = 200 * time.Millisecond
	}
	if o.SnapshotThreshold != 0 {
		c.SnapshotThreshold = o.SnapshotThreshold
		c.TrailingLogs = o.SnapshotThreshold / 2
	}
	return c
}

// OpenFresh starts Raft on an empty data directory so a leader can add this node
// (`tailraid join`): the log fills by replication or snapshot install.
func OpenFresh(o Options) (*Node, error) {
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
	return start(o, st, trans)
}

func start(o Options, st *stores, trans raft.Transport) (*Node, error) {
	log, logOutput := o.logger()
	fsm := meta.NewFSM()
	r, err := raft.NewRaft(o.raftConfig(logOutput), fsm, st.logs, st.logs, st.snaps, trans)
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
	// Raft's AppliedIndex is advanced by its main goroutine when a batch is handed to
	// the FSM goroutine, not when the FSM has applied it, so it only proves dispatch.
	// Configuration entries never reach the FSM, so the FSM's own index is compared
	// against the last *command* entry in the log.
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for n.Raft.AppliedIndex() < n.Raft.LastIndex() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("cluster: log replay incomplete after %s (dispatched %d of %d)", timeout, n.Raft.AppliedIndex(), n.Raft.LastIndex())
		case <-tick.C:
		}
	}
	if n.IsLeader() {
		if err := n.Raft.Barrier(timeout).Error(); err != nil {
			return fmt.Errorf("cluster: barrier: %w", err)
		}
	}
	last, err := n.lastCommandIndex()
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := n.FSM.WaitApplied(wctx, last); err != nil {
		return fmt.Errorf("cluster: fsm replay incomplete (applied %d, last command %d): %w", n.FSM.Applied(), last, err)
	}
	return nil
}

// lastCommandIndex scans the log store backwards for the newest command entry. Entries
// compacted into a snapshot are already reflected in the FSM's applied index.
func (n *Node) lastCommandIndex() (uint64, error) {
	first, err := n.logs.FirstIndex()
	if err != nil {
		return 0, fmt.Errorf("cluster: first index: %w", err)
	}
	for idx := n.Raft.LastIndex(); idx >= first && idx > 0; idx-- {
		var l raft.Log
		if err := n.logs.GetLog(idx, &l); err != nil {
			if errors.Is(err, raft.ErrLogNotFound) {
				// compacted into a snapshot the FSM has already restored
				return 0, nil
			}
			return 0, fmt.Errorf("cluster: read log %d: %w", idx, err)
		}
		if l.Type == raft.LogCommand {
			return idx, nil
		}
	}
	return 0, nil
}

// IsLeader reports whether this node currently leads.
func (n *Node) IsLeader() bool { return n.Raft.State() == raft.Leader }

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
