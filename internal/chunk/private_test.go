package chunk

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
)

func wantMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s is %04o, want %04o", path, got, want)
	}
}

// A store opened on a tree an older release made 0755 tightens the shards, quarantine
// and prefix directories to 0700; a prefix a new shard creates is 0700 and the shard 0600.
// Invariant: I-340
func TestStore_DirsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits worth reading")
	}
	root := t.TempDir()
	for _, d := range []string{"shards", "shards/ab", "quarantine"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(sysfs.OS{}, root, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"shards", "shards/ab", "quarantine"} {
		wantMode(t, filepath.Join(root, d), 0o700)
	}
	h := sha256.Sum256([]byte("private"))
	k := Key(h, 2, 1, 0)
	if _, err := s.Put(k, []byte("payload at a non-zero offset in the file: the header comes first")); err != nil {
		t.Fatal(err)
	}
	wantMode(t, filepath.Dir(s.path(k)), 0o700)
	wantMode(t, s.path(k), 0o600)
}

// The chunk cache does the same for its directory and the prefixes under it.
// Invariant: I-340
func TestCache_DirsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits worth reading")
	}
	root := t.TempDir()
	for _, d := range []string{"cache", "cache/01"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c, err := OpenCache(sysfs.OS{}, root, 1<<20, clock.NewFake(time.Unix(1_700_000_000, 0)))
	if err != nil {
		t.Fatal(err)
	}
	wantMode(t, filepath.Join(root, "cache"), 0o700)
	wantMode(t, filepath.Join(root, "cache", "01"), 0o700)
	h := sha256.Sum256([]byte("cached"))
	c.Put(h, []byte("a chunk"))
	c.Drain()
	p := c.path(h)
	wantMode(t, filepath.Dir(p), 0o700)
	wantMode(t, p, 0o600)
}
