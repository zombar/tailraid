package chunk

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
)

// A cache temp file that a crash left behind is removed when the cache opens, like the
// shard store's (L-127); it never counts toward the cache's size.
// Invariant: I-11
func TestCache_OpenRemovesTempFiles(t *testing.T) {
	root := t.TempDir()
	c, err := OpenCache(sysfs.OS{}, root, 1<<20, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Drain()
	p := payload(1000, 1)
	h := sha256.Sum256(p)
	c.Put(h, p)
	c.Drain()
	stale := filepath.Join(filepath.Dir(c.path(h)), ".tmp-leftover")
	if err := os.WriteFile(stale, []byte("half a chunk"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := OpenCache(sysfs.OS{}, root, 1<<20, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Drain()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("temp file still present after open: %v", err)
	}
	if c2.Used() != 1000 {
		t.Fatalf("used %d, want the one real entry (1000)", c2.Used())
	}
	if _, ok := c2.Get(h); !ok {
		t.Fatal("real entry lost")
	}
}
