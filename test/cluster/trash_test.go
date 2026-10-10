package cluster_test

import (
	"bytes"
	"errors"
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
