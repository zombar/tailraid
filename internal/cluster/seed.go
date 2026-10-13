package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
)

// seedFile holds the device members a join response listed, so that `serve` on a node
// whose log has not arrived yet accepts the leader's Raft connection (I-92): the member
// check on accept has nothing else to go by until the first entries are applied. It is
// removed once the node is ready with its table, and by reset.
const seedFile = "members.seed"

// WriteMemberSeed persists the join response's device members under dataDir.
func WriteMemberSeed(dataDir string, members []meta.Node) error {
	var devices []meta.Node
	for i := range members {
		if m := &members[i]; m.Kind == meta.KindDevice && m.State != meta.Removed && m.Addr != "" {
			devices = append(devices, *m)
		}
	}
	b, err := json.Marshal(devices)
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, seedFile), b)
}

// LoadMemberSeed reads the persisted seed; a missing file is no seed and no error.
func LoadMemberSeed(dataDir string) ([]meta.Node, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, seedFile)) //nolint:gosec // the seed in this node's data directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var members []meta.Node
	if err := json.Unmarshal(b, &members); err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", seedFile, err)
	}
	return members, nil
}

// ForgetMemberSeed removes the seed: the member table has taken over.
func ForgetMemberSeed(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, seedFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// StartSeed is the member seed serve's authorizer starts with, before the leader has
// connected (I-92, I-281): the table as of this node's own log, which is the restored
// table with the entries past its index replayed on a copy, plus the members of the seed
// file the log does not mention at all (the join target, a seed from before the log). A
// row the log removed stays out. The restored table alone predates every join since its
// snapshot, and the leader may be one of those joiners.
func (n *Node) StartSeed(restored *meta.FSM, file []meta.Node) ([]meta.Node, error) {
	last, err := n.logs.LastIndex()
	if err != nil {
		return nil, fmt.Errorf("cluster: last index: %w", err)
	}
	asOf, err := cloneFSM(restored)
	if err != nil {
		return nil, err
	}
	for idx := asOf.Applied() + 1; idx <= last; idx++ {
		var l raft.Log
		if err := n.logs.GetLog(idx, &l); err != nil {
			if errors.Is(err, raft.ErrLogNotFound) {
				continue
			}
			return nil, fmt.Errorf("cluster: read log %d: %w", idx, err)
		}
		if l.Type == raft.LogCommand {
			asOf.Apply(&l)
		}
	}
	known := map[string]bool{}
	var out []meta.Node
	nodes := asOf.Nodes()
	for i := range nodes {
		m := &nodes[i]
		known[m.ID] = true
		if m.Kind == meta.KindDevice && m.State != meta.Removed && m.Addr != "" {
			out = append(out, *m)
		}
	}
	for i := range file {
		if !known[file[i].ID] {
			out = append(out, file[i])
		}
	}
	return out, nil
}

// cloneFSM copies f's state into a fresh FSM (no hooks), through the snapshot encoding.
func cloneFSM(f *meta.FSM) (*meta.FSM, error) {
	snap, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	st, ok := snap.(interface{ State() *meta.State })
	if !ok {
		return nil, errors.New("cluster: FSM snapshot without state")
	}
	var buf bytes.Buffer
	if err := meta.EncodeState(&buf, st.State()); err != nil {
		return nil, err
	}
	out := meta.NewFSM()
	if err := out.Restore(io.NopCloser(&buf)); err != nil {
		return nil, err
	}
	return out, nil
}
