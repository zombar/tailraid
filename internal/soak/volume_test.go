package soak_test

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"

	"github.com/zombar/tailraid/internal/soak"
)

// Hash streams a file to the same digest and size ReadAll would give, on both the
// mounted and the in-process volumes.
func TestVolume_Hash_MatchesReadAll(t *testing.T) {
	data := bytes.Repeat([]byte("large file bytes "), 70_000)
	want := sha256.Sum256(data)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	mem := soak.BillyVolume{FS: memfs.New()}
	if err := mem.WriteAt("/f", 0, data); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]soak.Volume{"os": soak.OSVolume{Root: root}, "billy": mem} {
		sum, n, err := v.Hash("/f")
		if err != nil || n != int64(len(data)) || sum != want {
			t.Fatalf("%s: hash %x size %d err %v", name, sum[:4], n, err)
		}
		all, err := v.ReadAll("/f")
		if err != nil || sha256.Sum256(all) != sum {
			t.Fatalf("%s: ReadAll disagrees", name)
		}
		if _, _, err := v.Hash("/missing"); err == nil {
			t.Fatalf("%s: hash of a missing file succeeded", name)
		}
	}
}
