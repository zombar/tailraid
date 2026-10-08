package sysfs_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/sysfs"
)

// The OS implementation is thin; these tests pin the semantics the shard store relies on:
// CreateTemp lands in the requested dir, Rename is atomic replace, SyncDir works on a dir,
// ReadDir lists temp files (so the store can sweep them), Chtimes moves mtime.
func TestOS_TempWriteSyncRenameSyncDir(t *testing.T) {
	var fsys sysfs.FS = sysfs.OS{}
	dir := t.TempDir()
	f, err := fsys.CreateTemp(dir, ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(f.Name()) != dir {
		t.Fatalf("temp not in dir: %s", f.Name())
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, "final")
	if err := os.WriteFile(final, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Rename(f.Name(), final); err != nil {
		t.Fatal(err)
	}
	if err := fsys.SyncDir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != "hello" {
		t.Fatalf("rename did not replace: %q %v", got, err)
	}
	entries, err := fsys.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "final" {
		t.Fatalf("ReadDir = %v %v", entries, err)
	}
	st, err := fsys.Stat(final)
	if err != nil || st.Size() != 5 {
		t.Fatalf("Stat = %v %v", st, err)
	}
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := fsys.Chtimes(final, past, past); err != nil {
		t.Fatal(err)
	}
	st, _ = fsys.Stat(final)
	if !st.ModTime().Equal(past) {
		t.Fatalf("Chtimes mtime = %v", st.ModTime())
	}
	rf, err := fsys.Open(final)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if n, err := rf.ReadAt(buf, 3); err != nil && !errors.Is(err, io.EOF) || n != 2 || string(buf) != "lo" {
		t.Fatalf("ReadAt = %d %q %v", n, buf, err)
	}
	all, err := io.ReadAll(rf)
	if err != nil || string(all) != "hello" {
		t.Fatalf("Read = %q %v", all, err)
	}
	_ = rf.Close()
	if err := fsys.Remove(final); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(final); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat after Remove = %v", err)
	}
	if err := fsys.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st, err := fsys.Stat(filepath.Join(dir, "a", "b")); err != nil || !st.IsDir() {
		t.Fatalf("MkdirAll: %v %v", st, err)
	}
}

func TestOS_ReadDirMissingIsErrNotExist(t *testing.T) {
	_, err := sysfs.OS{}.ReadDir(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}
