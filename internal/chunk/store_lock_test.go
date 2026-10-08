package chunk

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/sysfs"
)

// gatedFS blocks the Sync of any file created under hold until release is closed, and
// counts MkdirAll calls.
type gatedFS struct {
	sysfs.FS
	hold    string
	release chan struct{}
	entered chan struct{}
	mkdirs  atomic.Int32
}

func (g *gatedFS) MkdirAll(name string, perm fs.FileMode) error {
	g.mkdirs.Add(1)
	return g.FS.MkdirAll(name, perm)
}

func (g *gatedFS) CreateTemp(dir, pattern string) (sysfs.File, error) {
	f, err := g.FS.CreateTemp(dir, pattern)
	if err != nil || dir != g.hold {
		return f, err
	}
	return gatedFile{f, g}, nil
}

type gatedFile struct {
	sysfs.File
	g *gatedFS
}

func (f gatedFile) Sync() error {
	select {
	case <-f.g.entered:
	default:
		close(f.g.entered)
	}
	<-f.g.release
	return f.File.Sync()
}

// A PUT whose fsync is stalled holds up PUTs of the same key only: a shard of another key
// lands meanwhile, and a prefix directory is created once per process, not per PUT.
func TestStore_Put_DifferentKeysDoNotSerialize(t *testing.T) {
	a := Key(sha256.Sum256([]byte("a")), 2, 1, 0)
	var b ShardKey
	for i := 0; ; i++ { // a key in another prefix directory
		b = Key(sha256.Sum256([]byte{byte(i)}), 2, 1, 0)
		if b[:2] != a[:2] {
			break
		}
	}
	root := t.TempDir()
	st0, _ := Open(sysfs.OS{}, root, 0)
	g := &gatedFS{FS: sysfs.OS{}, hold: filepath.Dir(st0.path(a)), release: make(chan struct{}), entered: make(chan struct{})}
	st, err := Open(g, root, 0)
	if err != nil {
		t.Fatal(err)
	}
	g.mkdirs.Store(0)
	slow := make(chan error, 1)
	go func() { _, err := st.Put(a, payload(100, 1)); slow <- err }()
	<-g.entered // a's fsync is stalled
	fast := make(chan error, 1)
	go func() { _, err := st.Put(b, payload(100, 2)); fast <- err }()
	select {
	case err := <-fast:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(g.release)
		t.Fatal("a PUT of another key waited behind a stalled fsync")
	}
	// the same key does wait: a second PUT of a cannot run before the first finishes
	same := make(chan error, 1)
	go func() { _, err := st.Put(a, payload(100, 1)); same <- err }()
	select {
	case <-same:
		t.Fatal("a PUT of the same key ran concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(g.release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	if err := <-same; err != nil {
		t.Fatal(err)
	}
	if n := g.mkdirs.Load(); n != 2 { // one per prefix directory, not one per PUT (three PUTs)
		t.Fatalf("MkdirAll called %d times", n)
	}
	if _, err := st.Put(b, payload(100, 3)); err != nil {
		t.Fatal(err)
	}
	if n := g.mkdirs.Load(); n != 2 {
		t.Fatalf("MkdirAll called %d times after a fourth PUT", n)
	}
}

// PutVerified trusts the caller's sum (it was verified at the hop); a wrong sum is caught
// by the next Get, never served.
func TestStore_PutVerified_WrongSumIsCorrupt(t *testing.T) {
	st, _ := Open(sysfs.OS{}, t.TempDir(), 0)
	p := payload(500, 4)
	k := Key(sha256.Sum256(p), 1, 1, 0)
	if _, err := st.PutVerified(k, p, sha256.Sum256([]byte("not the payload"))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(k); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get after a wrong sum: %v", err)
	}
	good := Key(sha256.Sum256(p), 1, 1, 1)
	if _, err := st.PutVerified(good, p, sha256.Sum256(p)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Get(good); err != nil || len(got) != 500 {
		t.Fatalf("Get after the right sum: %d %v", len(got), err)
	}
}
