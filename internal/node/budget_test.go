package node

import (
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/sysfs"
)

// The budget init, join and serve register is pinned by node.capacity, otherwise derived
// from the data disk and set as the store's cap (docs/01 §Node budget).
// Invariant: I-74
func TestBudget_PinnedOrDerived(t *testing.T) {
	cfg := config.Default()
	cfg.Node.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Node.Capacity = "3G"
	if b, err := Budget(cfg); err != nil || b != 3<<30 {
		t.Fatalf("pinned: %d %v", b, err)
	}
	store, pinned, err := openStore(cfg)
	if err != nil || !pinned || store.Capacity() != 3<<30 {
		t.Fatalf("pinned store: cap %d pinned %v %v", store.Capacity(), pinned, err)
	}
	cfg.Node.Capacity = ""
	free, total, err := sysfs.OS{}.Free(cfg.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	want := chunk.Budget(0, free, total)
	b, err := Budget(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// the disk breathes between the two measurements; allow a sliver either way
	if diff := int64(b) - int64(want); diff > 64<<20 || diff < -64<<20 {
		t.Fatalf("derived %d, want about %d (free %d of %d)", b, want, free, total)
	}
	store, pinned, err = openStore(cfg)
	if err != nil || pinned || store.Capacity() <= 0 {
		t.Fatalf("derived store: cap %d pinned %v %v", store.Capacity(), pinned, err)
	}
}
