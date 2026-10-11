package cluster

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// gatedLogs is a log store whose entry `at` is handed out only once gate is closed, so a
// leader elected from this log holds its replay there: its FSM lags its log for as long
// as the test wants, as a slow disk makes it for a moment after every restart.
type gatedLogs struct {
	raft.LogStore
	at   uint64
	gate chan struct{}
	seen atomic.Int32
}

// GetLog lets Open's configuration scan read the entry and holds every later read: the
// leader's replay after its election.
func (g *gatedLogs) GetLog(index uint64, out *raft.Log) error {
	if index == g.at && g.seen.Add(1) > 1 {
		<-g.gate
	}
	return g.LogStore.GetLog(index, out)
}

// lockedBuf collects the node's log lines for the test to grep.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A member op reads the row as of every entry the leader has committed. A leader elected
// before its FSM replayed its log (a node restarted from a snapshot older than its log)
// would otherwise read the snapshot's copy of the row and, since an upsert replaces the
// whole row, a set_version reported at that moment would revert every field changed
// since the snapshot (CI, 2026-10-11: the budget a test had put on a device row).
// Invariant: I-177, I-133
func TestMemberOp_ReadsAppliedRow(t *testing.T) {
	dir := t.TempDir()
	st := raft.NewInmemStore()
	stores := &Stores{Logs: st, Stable: st, Snaps: raft.NewInmemSnapshotStore()}
	o := opts(t, dir)
	o.Stores = stores
	n, err := Bootstrap(ctx, o, meta.Node{Name: "solo", Kind: meta.KindDevice}, meta.SetVolume{ChunkSize: 1 << 20, K: 1, M: 0, Now: 1}, false, "token")
	if err != nil {
		t.Fatal(err)
	}
	must(t, n.Raft.Snapshot().Error())
	self, _ := n.SelfIdx()
	row, _ := n.FSM.Node(self)
	row.Name = "renamed" // after the snapshot: the next start learns it from the log alone
	_, renamed, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: row}})
	must(t, err)
	// one entry behind it: Open reads the last entry of the log before any election
	_, _, err = n.Propose(ctx, meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "d", Mode: 0o755, Now: 2}})
	must(t, err)
	must(t, n.Close())

	gate := make(chan struct{})
	var open sync.Once
	release := func() { open.Do(func() { close(gate) }) }
	defer release()
	stores.Logs = &gatedLogs{LogStore: st, at: renamed, gate: gate}
	var logs lockedBuf
	o2 := opts(t, dir)
	o2.Stores = stores
	o2.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	n2, err := Open(o2)
	must(t, err)
	defer n2.Close()
	must(t, n2.WaitLeader(ctx, 10*time.Second))
	done := make(chan error, 1)
	go func() { done <- n2.MemberOp(ctx, rpc.MemberOp{Op: "set_version", NodeIdx: self, Version: "v2"}) }()
	// a stale read happens at once and logs the op before the replay can proceed; the op
	// that waits for its log logs nothing until the gate opens
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "member op") && time.Now().Before(deadline) {
		<-time.After(10 * time.Millisecond)
	}
	release()
	must(t, <-done)
	must(t, n2.WaitReady(ctx, 10*time.Second))
	got, _ := n2.FSM.Node(self)
	if got.Name != "renamed" || got.Version != "v2" {
		t.Fatalf("row after set_version: name %q version %q (a stale read reverted the rename)", got.Name, got.Version)
	}
}
