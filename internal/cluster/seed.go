package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
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

// MembersFromLog replays the command entries this node's Raft log already holds into a
// scratch FSM and returns the device members of its table. A follower whose table has
// not arrived and that has no seed file (it joined before the seed existed, or the file
// was lost) would otherwise refuse the leader's connection forever and never receive the
// table. The entries in its log were sent by the leader the join accepted, so the
// members they name are ones that leader vouched for, as the seed's are (I-134). Entries
// already compacted into a snapshot are skipped: the FSM was restored from it at open.
func (n *Node) MembersFromLog() ([]meta.Node, error) {
	first, err := n.logs.FirstIndex()
	if err != nil {
		return nil, fmt.Errorf("cluster: first index: %w", err)
	}
	last, err := n.logs.LastIndex()
	if err != nil {
		return nil, fmt.Errorf("cluster: last index: %w", err)
	}
	scratch := meta.NewFSM()
	for idx := max(first, 1); idx <= last; idx++ {
		var l raft.Log
		if err := n.logs.GetLog(idx, &l); err != nil {
			if errors.Is(err, raft.ErrLogNotFound) {
				continue
			}
			return nil, fmt.Errorf("cluster: read log %d: %w", idx, err)
		}
		if l.Type == raft.LogCommand {
			scratch.Apply(&l)
		}
	}
	var out []meta.Node
	nodes := scratch.Nodes()
	for i := range nodes {
		if m := &nodes[i]; m.Kind == meta.KindDevice && m.State != meta.Removed && m.Addr != "" {
			out = append(out, *m)
		}
	}
	return out, nil
}
