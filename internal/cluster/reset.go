package cluster

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// ErrDirtyWAL is returned by Reset while staging/ holds write-ahead logs: those are
// acknowledged bytes that have not been flushed to the cluster.
var ErrDirtyWAL = errors.New("cluster: staging holds unflushed writes; start serve to flush them, or reset --discard-dirty to lose them")

// Reset wipes this device's local Raft state and identity so it can join afresh (docs/02
// §Lifecycle commands): <data_dir>/raft, node_id and cluster_id are deleted. shards/ and
// cache/ are never touched (I-13; the sweep reconciles shards after the rejoin). Reset
// refuses while serve holds the data directory, and while staging/ holds unflushed WALs
// unless discardDirty, in which case staging/ is deleted too.
func Reset(dataDir string, discardDirty bool) error {
	if HasState(dataDir) {
		logs, err := raftboltdb.New(raftboltdb.Options{
			Path:        filepath.Join(dataDir, "raft", "raft.db"),
			BoltOptions: &bbolt.Options{Timeout: time.Second},
		})
		if err != nil {
			if errors.Is(err, bolterrors.ErrTimeout) {
				return ErrAlreadyOpen
			}
			return fmt.Errorf("cluster: open raft.db: %w", err)
		}
		_ = logs.Close()
	}
	wals, _ := filepath.Glob(filepath.Join(dataDir, "staging", "*.wal"))
	if len(wals) > 0 && !discardDirty {
		return fmt.Errorf("%w (%d logs)", ErrDirtyWAL, len(wals))
	}
	for _, p := range []string{filepath.Join(dataDir, "raft"), filepath.Join(dataDir, "node_id"), filepath.Join(dataDir, "cluster_id"), filepath.Join(dataDir, seedFile)} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("cluster: reset %s: %w", p, err)
		}
	}
	if discardDirty {
		if err := os.RemoveAll(filepath.Join(dataDir, "staging")); err != nil {
			return fmt.Errorf("cluster: reset staging: %w", err)
		}
	}
	return nil
}
