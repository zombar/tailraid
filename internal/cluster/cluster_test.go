package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/meta"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var ctx = context.Background()

func opts(t testing.TB, dir string) Options {
	t.Helper()
	id, err := LoadOrCreateNodeID(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, trans := raft.NewInmemTransport(raft.ServerAddress(id))
	return Options{DataDir: dir, NodeID: id, Transport: trans, Timing: &RaftTiming{Heartbeat: time.Second, Election: time.Second, LeaderLease: 500 * time.Millisecond, Commit: 10 * time.Millisecond, SnapshotInterval: 500 * time.Millisecond}}
}

func bootstrap(t testing.TB, dir string) *Node {
	t.Helper()
	n, err := Bootstrap(ctx, opts(t, dir), meta.Node{Name: "solo", Kind: meta.KindDevice, Capacity: 1 << 30},
		meta.SetVolume{ChunkSize: 1 << 20, K: 1, M: 0, Now: 1}, false, "token")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Invariant: I-5
// Lifecycle: L-120
func TestSingleNode_BootstrapProposeRestart(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	if !n.IsLeader() {
		t.Fatal("single voter is not leader")
	}
	if v := n.FSM.Volume(); v.K != 1 || v.ChunkSize != 1<<20 {
		t.Fatalf("volume %+v", v)
	}
	if idx, ok := n.SelfIdx(); !ok || idx != 1 {
		t.Fatalf("self idx %d %v", idx, ok)
	}
	if m := n.FSM.Meta(); m.JoinToken != "token" || m.ClusterID == [16]byte{} {
		t.Fatalf("meta %+v", m)
	}
	res, index, err := n.Propose(ctx, meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: meta.RootIno, Name: "f", Type: meta.TFile, Mode: 0o644, Now: 2}})
	if err != nil || res.Err != nil || index == 0 {
		t.Fatalf("propose: %v %v %d", err, res.Err, index)
	}
	if err := n.FSM.WaitApplied(ctx, index); err != nil {
		t.Fatal(err)
	}
	ino := res.Ino
	// a rejected command comes back in Result.Err, not err
	res, _, err = n.Propose(ctx, meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: meta.RootIno, Name: "f", Type: meta.TFile, Now: 3}})
	if err != nil || !errors.Is(res.Err, meta.ErrExist) {
		t.Fatalf("dup create: %v %v", err, res.Err)
	}
	if _, err := Bootstrap(ctx, opts(t, dir), meta.Node{}, meta.SetVolume{}, false, ""); !errors.Is(err, ErrAlreadyInit) {
		t.Fatalf("second bootstrap: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	// restart from disk: the log replays into a fresh FSM
	n2, err := Open(opts(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Close()
	if err := n2.WaitLeader(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := n2.FSM.WaitApplied(ctx, index); err != nil {
		t.Fatal(err)
	}
	in, ok := n2.FSM.GetInode(ino)
	if !ok || in.Name != "f" {
		t.Fatalf("inode after restart: %+v %v", in, ok)
	}
	if n2.ID != n.ID {
		t.Fatal("node id changed")
	}
}

// Regression: the mount test found serve reading the node table before a restarted
// leader had replayed its log ("this node is not in the cluster node table").
// Lifecycle: L-120
func TestOpen_WaitReadyReplaysLogBeforeReads(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	for i := range 50 { // enough entries that replay is observable
		_, _, _ = n.Propose(ctx, meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: string(rune('a'+i%26)) + string(rune('a'+i/26)), Mode: 0o755, Now: int64(i)}})
	}
	must(t, n.Close())
	for round := range 3 {
		n2, err := Open(opts(t, dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := n2.WaitReady(ctx, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, ok := n2.SelfIdx(); !ok {
			t.Fatalf("round %d: node table empty after WaitReady", round)
		}
		if st := n2.FSM.Stats(); st.Inodes != 51 {
			t.Fatalf("round %d: %d inodes after replay", round, st.Inodes)
		}
		must(t, n2.Close())
	}
}

// A follower knows the leader from its first heartbeat, before the entry that adds its
// row: Join adds the Raft server, then proposes the row. In that window WaitReady holds
// (a leader, the local log applied) while the table has no row for the node; WaitMember
// waits for the row and fails with ErrNotMember when it never comes.
// Regression: node0's first serve after its join, 2026-10-10
// Invariant: I-144
func TestWaitMember_WaitsForOwnRow(t *testing.T) {
	leader := bootstrap(t, t.TempDir())
	defer leader.Close()
	fo := opts(t, t.TempDir())
	follower, err := OpenFresh(fo)
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	la, fa := leader.trans.LocalAddr(), fo.Transport.LocalAddr()
	leader.trans.(*raft.InmemTransport).Connect(fa, fo.Transport)
	fo.Transport.(*raft.InmemTransport).Connect(la, leader.trans)
	must(t, leader.Raft.AddNonvoter(raft.ServerID(fo.NodeID), fa, 0, 10*time.Second).Error())
	must(t, follower.WaitReady(ctx, 10*time.Second))
	if _, ok := follower.SelfIdx(); ok {
		t.Fatal("the follower has a row before the leader proposed one")
	}
	if err := follower.WaitMember(ctx, 300*time.Millisecond); !errors.Is(err, ErrNotMember) {
		t.Fatalf("WaitMember without a row: %v, want ErrNotMember", err)
	}
	done := make(chan error, 1)
	go func() { done <- follower.WaitMember(ctx, 10*time.Second) }()
	row := meta.Node{ID: fo.NodeID, Name: "two", Kind: meta.KindDevice, Addr: "10.0.0.2", RaftPort: 1, RPCPort: 2}
	if _, _, err := leader.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: row}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WaitMember once the row is committed: %v", err)
	}
	if _, ok := follower.SelfIdx(); !ok || follower.FSM.Volume().ChunkSize == 0 {
		t.Fatal("WaitMember returned without the row and the volume")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpen_RequiresInit(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(opts(t, dir)); !errors.Is(err, ErrNotInit) {
		t.Fatalf("open uninitialised: %v", err)
	}
}

func TestSecondOpen_IsRefused(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	defer n.Close()
	o := opts(t, dir)
	if _, err := Open(o); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("second open: %v", err)
	}
}

func TestLoadOrCreateNodeID_Stable(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateNodeID(dir)
	if err != nil || len(a) != 16 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := LoadOrCreateNodeID(dir)
	if a != b {
		t.Fatal("node id not stable")
	}
}

// A node whose raft.db cannot be opened fails to start with a clear error instead of
// panicking or starting empty; the rest of the cluster is unaffected (it never sees it).
// Lifecycle: L-124
func TestOpen_CorruptRaftDB_FailsClearly(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "raft", "raft.db")
	if err := os.WriteFile(db, []byte("this is not a bolt database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(opts(t, dir))
	if err == nil {
		t.Fatal("open succeeded on a corrupt raft.db")
	}
	if !strings.Contains(err.Error(), "raft.db") {
		t.Fatalf("error does not name the file: %v", err)
	}
}
