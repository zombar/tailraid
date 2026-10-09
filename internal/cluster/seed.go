package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
