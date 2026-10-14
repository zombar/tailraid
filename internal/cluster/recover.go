package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// ErrNoSnapshot is an export of a node that applied nothing since it started and holds
// no snapshot either: there is no state to write yet.
var ErrNoSnapshot = errors.New("cluster: no snapshot to export yet")

// ExportSnapshot takes a Raft snapshot and writes its bytes to dir/<id>/state.bin with
// the snapshot's meta.json beside it, each fsynced before its rename (docs/12 §9,
// I-129). It returns the directory. Raft refuses a snapshot ("nothing new") from a node
// whose FSM applied nothing since the process started, which a restarted follower is
// until the next write; its state is then the newest snapshot the store holds, which the
// start restored, so that one is exported. The state is the metadata only: shards stay
// where they are.
func (n *Node) ExportSnapshot(dir string) (string, raft.SnapshotMeta, error) {
	f := n.Raft.Snapshot()
	err := f.Error()
	if errors.Is(err, raft.ErrNothingNewToSnapshot) {
		return exportNewest(n.snaps, dir)
	}
	if err != nil {
		return "", raft.SnapshotMeta{}, fmt.Errorf("cluster: snapshot: %w", err)
	}
	m, rc, err := f.Open()
	if err != nil {
		return "", raft.SnapshotMeta{}, fmt.Errorf("cluster: open snapshot: %w", err)
	}
	defer rc.Close()
	out, err := writeExport(dir, m, rc)
	return out, *m, err
}

// exportNewest exports the newest snapshot of the store.
func exportNewest(snaps raft.SnapshotStore, dir string) (string, raft.SnapshotMeta, error) {
	list, err := snaps.List()
	if err != nil {
		return "", raft.SnapshotMeta{}, fmt.Errorf("cluster: list snapshots: %w", err)
	}
	if len(list) == 0 {
		return "", raft.SnapshotMeta{}, ErrNoSnapshot
	}
	m := list[0] // newest first
	_, rc, err := snaps.Open(m.ID)
	if err != nil {
		return "", raft.SnapshotMeta{}, fmt.Errorf("cluster: open snapshot %s: %w", m.ID, err)
	}
	defer rc.Close()
	out, err := writeExport(dir, m, rc)
	return out, *m, err
}

// writeExport writes a snapshot's bytes and meta.json under dir/<id>.
func writeExport(dir string, m *raft.SnapshotMeta, rc io.Reader) (string, error) {
	out := filepath.Join(dir, m.ID)
	if err := os.MkdirAll(out, 0o700); err != nil {
		return "", err
	}
	if err := streamFileSync(filepath.Join(out, "state.bin"), rc); err != nil {
		return "", err
	}
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(out, "meta.json"), mj); err != nil {
		return "", err
	}
	return out, nil
}

// streamFileSync writes r to path through a temp file, fsync and rename.
func streamFileSync(path string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return sysfs.OS{}.SyncDir(filepath.Dir(path))
}

// ErrNotVoter is a Recover whose voter list leaves this node out.
var ErrNotVoter = errors.New("cluster: this node must be one of the recovered voters")

// Recover rewrites the Raft membership held in o's stores to voters (docs/12 §9, I-129):
// hashicorp/raft's RecoverCluster replays the newest snapshot and the log after it into
// a fresh FSM, writes a snapshot that carries the new configuration and truncates the
// log, so the next Open starts with exactly these voters and all the metadata the node
// had. Nothing runs while it works: it refuses stores another process holds
// (ErrAlreadyOpen), a node without state (ErrNotInit) and a list without this node
// (ErrNotVoter).
func Recover(o Options, voters []raft.Server) error {
	if !o.hasState() {
		return ErrNotInit
	}
	self := false
	for _, v := range voters {
		if string(v.ID) == o.NodeID {
			self = true
		}
	}
	if !self {
		return ErrNotVoter
	}
	_, logOutput := o.logger()
	st, err := o.openStores(logOutput)
	if err != nil {
		return err
	}
	defer st.close()
	_, trans := raft.NewInmemTransport(raft.ServerAddress(o.NodeID))
	defer trans.Close()
	if err := raft.RecoverCluster(o.raftConfig(logOutput), meta.NewFSM(), st.logs, st.stable, st.snaps, trans, raft.Configuration{Servers: voters}); err != nil {
		return fmt.Errorf("cluster: recover: %w", err)
	}
	return nil
}

// OfflineMembers reads the member table as the stores hold it (the newest snapshot
// plus the committed log after it) without starting Raft, so `tailraid recover` can
// turn voter IDs into Raft addresses on a stopped node.
func OfflineMembers(o Options) ([]meta.Node, error) {
	if !o.hasState() {
		return nil, ErrNotInit
	}
	_, logOutput := o.logger()
	st, err := o.openStores(logOutput)
	if err != nil {
		return nil, err
	}
	defer st.close()
	fsm := meta.NewFSM()
	from := uint64(1)
	if snaps, err := st.snaps.List(); err == nil && len(snaps) > 0 {
		m := snaps[0] // newest first
		_, rc, err := st.snaps.Open(m.ID)
		if err != nil {
			return nil, fmt.Errorf("cluster: open snapshot %s: %w", m.ID, err)
		}
		err = fsm.Restore(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("cluster: restore snapshot %s: %w", m.ID, err)
		}
		from = m.Index + 1
	}
	first, _ := st.logs.FirstIndex()
	last, _ := st.logs.LastIndex()
	from = max(from, first)
	for i := from; i <= last && i > 0; i++ {
		var l raft.Log
		if err := st.logs.GetLog(i, &l); err != nil {
			return nil, fmt.Errorf("cluster: log entry %d: %w", i, err)
		}
		if l.Type == raft.LogCommand {
			fsm.Apply(&l)
		}
	}
	return fsm.Nodes(), nil
}
