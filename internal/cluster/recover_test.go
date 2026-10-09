package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
)

// On a stopped node, Recover rewrites the voter set to the one given and the node opens
// again with every entry it had; it refuses a running node, an uninitialised directory
// and a voter list without this node. ExportSnapshot writes a decodable state file and
// its metadata.
//
// Invariant: I-129
func TestRecover_RewritesVotersOffline(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	res, index, err := n.Propose(ctx, meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: meta.RootIno, Name: "kept", Type: meta.TFile, Mode: 0o644, Now: 2}})
	if err != nil || res.Err != nil {
		t.Fatalf("propose: %v %v", err, res.Err)
	}
	if err := n.FSM.WaitApplied(ctx, index); err != nil {
		t.Fatal(err)
	}
	out, m, err := n.ExportSnapshot(filepath.Join(dir, "export"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(out, "state.bin"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := meta.DecodeState(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("decode exported state: %v", err)
	}
	if _, ok := st.Inodes[meta.RootIno].Children["kept"]; !ok {
		t.Fatal("the exported snapshot lacks the file")
	}
	if mj, err := os.ReadFile(filepath.Join(out, "meta.json")); err != nil || len(mj) == 0 || m.Index == 0 {
		t.Fatalf("meta.json: %v (index %d)", err, m.Index)
	}
	// a running node holds the stores: Recover is refused
	o := opts(t, dir)
	self := raft.Server{Suffrage: raft.Voter, ID: raft.ServerID(o.NodeID), Address: raft.ServerAddress(o.NodeID)}
	if err := Recover(o, []raft.Server{self}); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("recover while open: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// without this node, or on an empty directory: refused
	if err := Recover(o, []raft.Server{{Suffrage: raft.Voter, ID: "someone-else", Address: "x"}}); !errors.Is(err, ErrNotVoter) {
		t.Fatalf("recover without self: %v", err)
	}
	if err := Recover(opts(t, t.TempDir()), []raft.Server{self}); !errors.Is(err, ErrNotInit) {
		t.Fatalf("recover on an empty directory: %v", err)
	}
	members, err := OfflineMembers(o)
	if err != nil || len(members) != 1 || members[0].Name != "solo" {
		t.Fatalf("offline members: %v %+v", err, members)
	}
	if err := Recover(o, []raft.Server{self}); err != nil {
		t.Fatal(err)
	}
	n2, err := Open(opts(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Close()
	if err := n2.WaitReady(ctx, 10e9); err != nil {
		t.Fatal(err)
	}
	if !n2.IsLeader() {
		t.Fatal("the recovered sole voter does not lead")
	}
	if _, err := n2.FSM.Resolve([]string{"kept"}); err != nil {
		t.Fatalf("the file did not survive the recovery: %v", err)
	}
	cfg, err := n2.Raft.GetConfiguration().Error(), error(nil)
	if cfg != nil || err != nil {
		t.Fatal(cfg)
	}
	if servers := n2.Raft.GetConfiguration().Configuration().Servers; len(servers) != 1 || servers[0].ID != self.ID {
		t.Fatalf("configuration after recovery: %+v", servers)
	}
}
