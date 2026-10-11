package node

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// The config file's node.voter follows this node's row: a demoted node rewrites its
// file to false and a promoted one to true, a file that already agrees is left alone,
// a file that cannot be written is warned about and retried while serving goes on, and
// a node loaded without a file writes nothing.
// Invariant: I-176
func TestVoterFile_FollowsRow(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := c.Leader()
	nd := c.Nodes[(leader.I+1)%3]
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Node.Name, cfg.Node.DataDir, cfg.Node.Voter = "box", filepath.Join(dir, "data"), true
	p := filepath.Join(dir, "config.yaml")
	must(t, config.Save(p, cfg))
	cfg, err := config.Load(p)
	if err != nil || cfg.Path != p {
		t.Fatalf("load: %v, path %q", err, cfg.Path)
	}
	vf := &voterFile{cfg: cfg, voter: cfg.Node.Voter}
	log := slog.Default()
	voterInFile := func() bool {
		t.Helper()
		got, err := config.Load(p)
		must(t, err)
		return got.Node.Voter
	}
	// the row agrees: nothing written
	old := time.Now().Add(-time.Hour)
	must(t, os.Chtimes(p, old, old))
	vf.follow(nd.Node, nd.ID, log)
	if st, _ := os.Stat(p); !st.ModTime().Equal(old) || !voterInFile() {
		t.Fatal("a file that agreed with the row was rewritten")
	}
	// demoted on the leader: the file follows
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_voter", NodeIdx: nd.Idx(), Voter: false}))
	c.Settle()
	vf.follow(nd.Node, nd.ID, log)
	if voterInFile() {
		t.Fatal("the file still says voter: true after the demotion")
	}
	// promoted again: and back
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_voter", NodeIdx: nd.Idx(), Voter: true}))
	c.Settle()
	vf.follow(nd.Node, nd.ID, log)
	if !voterInFile() {
		t.Fatal("the file still says voter: false after the promotion")
	}
	// a file that cannot be written (a directory sits where Save writes its temp file,
	// which fails on every OS): warned, the file untouched, the next follow retries
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_voter", NodeIdx: nd.Idx(), Voter: false}))
	c.Settle()
	must(t, os.Mkdir(p+".tmp", 0o700))
	vf.follow(nd.Node, nd.ID, log)
	if !vf.voter {
		t.Fatal("an unwritable file was recorded as updated")
	}
	must(t, os.Remove(p+".tmp"))
	if !voterInFile() {
		t.Fatal("the file changed although it could not be written")
	}
	vf.follow(nd.Node, nd.ID, log)
	if voterInFile() {
		t.Fatal("the retry did not rewrite the file")
	}
	// a node with no config file: nothing to do, no panic
	none := &voterFile{voter: true}
	none.follow(nd.Node, nd.ID, log)
}
