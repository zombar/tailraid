package node

import (
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/sysfs"
)

// The budget a disk registers with is pinned by its capacity, otherwise derived from the
// filesystem holding it; a device row has no budget of its own (docs/01 §Node budget,
// docs/14 §3, §9).
// Invariant: I-74
func TestDiskBudget_PinnedOrDerived(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	if b, err := DiskBudget(config.Disk{Path: dir, Capacity: "3G"}); err != nil || b != 3<<30 {
		t.Fatalf("pinned: %d %v", b, err)
	}
	free, total, err := sysfs.OS{}.Free(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := chunk.Budget(0, free, total)
	b, err := DiskBudget(config.Disk{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	// the disk breathes between the two measurements; allow a sliver either way
	if diff := int64(b) - int64(want); diff > 64<<20 || diff < -64<<20 {
		t.Fatalf("derived %d, want about %d (free %d of %d)", b, want, free, total)
	}
	if _, err := DiskBudget(config.Disk{Path: dir, Capacity: "lots"}); err == nil {
		t.Fatal("a bad capacity was accepted")
	}
}
