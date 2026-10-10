package cluster_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// purgeTrash deletes everything in the trash for good through the leader, what
// `tailraid trash purge --all` does (docs/12 §10): the tests that watch GC reclaim a
// removed file need it, since a delete alone reclaims nothing (docs/06 §1).
func purgeTrash(t *testing.T, c *testharness.Cluster) {
	t.Helper()
	l := c.Leader()
	before := max(c.Clk.Now().UnixNano(), l.Node.FSM.LastNow()) + 1
	res, _, err := l.Node.Propose(ctx, meta.Command{Op: meta.OpPurge, Purge: &meta.Purge{Before: before, Now: c.Clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	must(t, err)
	c.Settle()
}

// A delete on one node, a restore proposed on another, the bytes read on a third: the
// trash is replicated state like any other.
//
// Invariant: I-152, I-155
func TestCluster_TrashRestoreAcrossNodes(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize)+100, 41)
	c.WriteFile(0, "/d/f", data)
	c.Settle()
	in, err := c.Nodes[0].Node.FSM.Resolve([]string{"d", "f"})
	must(t, err)
	must(t, c.Nodes[0].FS.Remove("/d/f"))
	c.Settle()
	for i := range c.Nodes {
		if _, err := c.Nodes[i].FS.Stat("/d/f"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("n%d still sees the deleted file: %v", i, err)
		}
		dir, err := c.Nodes[i].Node.FSM.Resolve([]string{"d"})
		must(t, err)
		if roots := c.Nodes[i].Node.FSM.TrashOf(dir.Ino); len(roots) != 1 || roots[0].Ino != in.Ino {
			t.Fatalf("n%d's trash: %+v", i, roots)
		}
	}
	// the bytes are readable from the trash on every node
	for i := range c.Nodes {
		fh, err := c.Nodes[i].FS.OpenTrash(in.Ino, nil)
		must(t, err)
		buf := make([]byte, len(data))
		n, _ := fh.ReadAt(buf, 0)
		_ = fh.Close()
		if !bytes.Equal(buf[:n], data) {
			t.Fatalf("n%d: trash read differs at %d", i, testharness.FirstDiff(buf[:n], data))
		}
	}
	res, _, err := c.Nodes[1].Node.Propose(ctx, meta.Command{Op: meta.OpRestore, Restore: &meta.Restore{Ino: in.Ino, Now: c.Clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	must(t, err)
	c.Settle()
	got, err := c.ReadFile(2, "/d/f")
	must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatalf("restored file read on n2 differs at %d", testharness.FirstDiff(got, data))
	}
	if back, err := c.Nodes[2].Node.FSM.Resolve([]string{"d", "f"}); err != nil || back.Ino != in.Ino || back.Gen != in.Gen {
		t.Fatalf("restored identity: %+v %v", back, err)
	}
}

// A leader refuses the codec-4 commands while a live member reports an older codec, and
// the gateway's delete falls back to the plain unlink; once every member reads codec 4
// the trash works.
//
// Invariant: I-156
func TestCluster_V4OpsRefusedUntilAllMembersUpgraded(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/old", testharness.Pattern(100, 1))
	c.WriteFile(0, "/new", testharness.Pattern(100, 2))
	c.Settle()
	leader := c.Leader()
	old, err := leader.Node.FSM.Resolve([]string{"old"})
	must(t, err)
	leader.Node.MinCodec = func() uint8 { return 3 } // one member still runs codec 3
	defer func() { leader.Node.MinCodec = nil }()
	res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: meta.RootIno, Name: "old", Now: c.Clk.Now().UnixNano()}})
	if err != nil || !errors.Is(res.Err, meta.ErrCodec) {
		t.Fatalf("Trash with an old member: %v %v, want ErrCodec", res.Err, err)
	}
	must(t, c.Nodes[leader.I].FS.Remove("/old")) // the gateway falls back to Unlink
	c.Settle()
	if _, ok := leader.Node.FSM.GetInodeAny(old.Ino); ok {
		t.Fatal("the fallback trashed the file")
	}
	if len(leader.Node.FSM.Trash()) != 0 {
		t.Fatal("something landed in the trash while a member was old")
	}
	leader.Node.MinCodec = func() uint8 { return meta.CodecVersion }
	must(t, c.Nodes[leader.I].FS.Remove("/new"))
	c.Settle()
	if len(leader.Node.FSM.Trash()) != 1 {
		t.Fatalf("trash after the update: %+v", leader.Node.FSM.Trash())
	}
}

// A node that missed the trash, a restore and a flag set catches up through a snapshot
// install, not the log: the rebuilt trash index, the stats and the flags on it equal
// the leader's, the trashed bytes read on it, and a restore proposed through it lands.
//
// Invariant: I-156
func TestCluster_TrashSurvivesSnapshotInstall(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1, testharness.SnapshotThreshold(40))
	defer c.Close()
	c.Kill(2)
	gone := testharness.Pattern(3000, 5)
	c.WriteFile(c.Leader().I, "/d/gone", gone)
	c.WriteFile(c.Leader().I, "/d/back", testharness.Pattern(100, 6))
	c.Settle()
	leader := c.Leader()
	goneIn, err := leader.Node.FSM.Resolve([]string{"d", "gone"})
	must(t, err)
	backIn, err := leader.Node.FSM.Resolve([]string{"d", "back"})
	must(t, err)
	dir, err := leader.Node.FSM.Resolve([]string{"d"})
	must(t, err)
	must(t, c.Nodes[leader.I].FS.Remove("/d/gone"))
	must(t, c.Nodes[leader.I].FS.Remove("/d/back"))
	c.Settle()
	for _, cmd := range []meta.Command{
		{Op: meta.OpRestore, Restore: &meta.Restore{Ino: backIn.Ino, Now: c.Clk.Now().UnixNano()}},
		{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: dir.Ino, Mask: meta.AttrFlags, Flags: meta.FlagProtected | meta.FlagNSFW, Now: c.Clk.Now().UnixNano()}},
	} {
		res, _, err := leader.Node.Propose(ctx, cmd)
		if err == nil {
			err = res.Err
		}
		must(t, err)
	}
	for i := range 200 { // past the snapshot threshold: the rejoining node installs a snapshot
		mkdirRetry(t, c, fmt.Sprintf("/d%03d", i))
	}
	c.Restart(2)
	c.Settle()
	n2 := c.Nodes[2].Node.FSM
	if got, want := n2.Stats(), leader.Node.FSM.Stats(); got.TrashInodes != 1 || got != want {
		t.Fatalf("rejoined stats %+v, leader %+v", got, want)
	}
	if roots := n2.TrashOf(dir.Ino); len(roots) != 1 || roots[0].Ino != goneIn.Ino || roots[0].DeletedAt == 0 {
		t.Fatalf("rejoined trash of /d: %+v", roots)
	}
	if in, err := n2.Resolve([]string{"d"}); err != nil || in.Flags != meta.FlagProtected|meta.FlagNSFW {
		t.Fatalf("rejoined /d: %+v %v", in, err)
	}
	if in, err := n2.Resolve([]string{"d", "back"}); err != nil || in.Ino != backIn.Ino {
		t.Fatalf("rejoined /d/back: %+v %v", in, err)
	}
	if _, ok := n2.GetInode(goneIn.Ino); ok {
		t.Fatal("the trashed root is live on the rejoined node")
	}
	fh, err := c.Nodes[2].FS.OpenTrash(goneIn.Ino, nil)
	must(t, err)
	buf := make([]byte, len(gone))
	n, _ := fh.ReadAt(buf, 0)
	_ = fh.Close()
	if !bytes.Equal(buf[:n], gone) {
		t.Fatalf("trashed bytes on the rejoined node differ at %d", testharness.FirstDiff(buf[:n], gone))
	}
	if v := meta.CheckInvariants(fsmState(t, n2)); len(v) != 0 {
		t.Fatalf("rejoined FSM invariants: %v", v)
	}
	// a restore through the rejoined node (forwarded to the leader) lands everywhere
	res, _, err := c.Nodes[2].Node.Propose(ctx, meta.Command{Op: meta.OpRestore, Restore: &meta.Restore{Ino: goneIn.Ino, Now: c.Clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	must(t, err)
	c.Settle()
	for i := range c.Nodes {
		got, err := c.ReadFile(i, "/d/gone")
		if err != nil || !bytes.Equal(got, gone) {
			t.Fatalf("n%d after restore: %v, differs at %d", i, err, testharness.FirstDiff(got, gone))
		}
	}
}

// fsmState is the FSM's state as its snapshot carries it.
func fsmState(t *testing.T, f *meta.FSM) *meta.State {
	t.Helper()
	snap, err := f.Snapshot()
	must(t, err)
	return snap.(interface{ State() *meta.State }).State()
}
