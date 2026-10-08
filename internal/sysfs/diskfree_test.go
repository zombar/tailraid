package sysfs

import (
	"os"
	"path/filepath"
	"testing"
)

// Invariant: I-74
func TestOS_Free(t *testing.T) {
	dir := t.TempDir()
	free, total, err := OS{}.Free(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || free == 0 || free > total {
		t.Fatalf("Free(%s) = %d of %d", dir, free, total)
	}
	if _, _, err := (OS{}).Free(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing dir: %v", err)
	}
}
