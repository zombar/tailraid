package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// reset wipes the local Raft state and identity and nothing else: shards and the cache
// survive, a running serve blocks it, and unflushed WALs block it unless discarded.
// Invariant: I-13
func TestReset_WipesRaftKeepsShards(t *testing.T) {
	dir := t.TempDir()
	n := bootstrap(t, dir)
	shard := filepath.Join(dir, "shards", "ab", "abcd.2-1.0")
	cache := filepath.Join(dir, "cache", "ab", "abcd")
	for _, p := range []string{shard, cache} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Reset(dir, false); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("reset while serve holds the directory: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// an unflushed WAL blocks the reset unless it is discarded on purpose
	wal := filepath.Join(dir, "staging", "7.wal")
	if err := os.MkdirAll(filepath.Dir(wal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wal, []byte("records"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Reset(dir, false); !errors.Is(err, ErrDirtyWAL) {
		t.Fatalf("reset with a dirty WAL: %v", err)
	}
	if !HasState(dir) {
		t.Fatal("a refused reset removed state")
	}
	if err := Reset(dir, true); err != nil {
		t.Fatal(err)
	}
	if HasState(dir) {
		t.Fatal("raft state survived")
	}
	for _, p := range []string{filepath.Join(dir, "node_id"), filepath.Join(dir, "cluster_id"), filepath.Join(dir, "staging")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived the reset", p)
		}
	}
	for _, p := range []string{shard, cache} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s did not survive the reset: %v", p, err)
		}
	}
	// a fresh identity: the device can bootstrap or join again
	id, err := LoadOrCreateNodeID(dir)
	if err != nil || len(id) != 16 {
		t.Fatalf("new node id %q: %v", id, err)
	}
	n2 := bootstrap(t, dir)
	_ = n2.Close()
}
