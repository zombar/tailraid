package vfs_test

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// The web UI's upload loop waits for the flusher when a cap is hit instead of failing
// (docs/13 §4), so the WAL and dirty-byte sentinels must stay reachable through the vfs
// error mapping while NFS keeps seeing EIO.
// Invariant: I-70
func TestVFS_CapErrorsKeepSentinels(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	f.RestartOverlay(vfs.WithMaxDirty(3000))
	fs := f.FS()
	write := func(name string, off int64, n int) error {
		fh, err := fs.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		defer fh.Close()
		if _, err := fh.Seek(off, io.SeekStart); err != nil {
			return err
		}
		_, err = fh.Write(testharness.Pattern(n, byte(n)))
		return err
	}
	must(t, write("/a", 4096, 3000))
	err := write("/a", 9000, 1)
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, vfs.ErrDirtyFull) {
		t.Fatalf("dirty cap: err=%v, want both EIO and ErrDirtyFull", err)
	}
	f.RestartOverlay(vfs.WithWALCap(1000))
	fs = f.FS()
	must(t, write("/w", 4096, 900))
	must(t, write("/w", 4096, 900))
	err = write("/w", 4096, 900)
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, vfs.ErrWALFull) {
		t.Fatalf("wal cap: err=%v, want both EIO and ErrWALFull", err)
	}
}

// Stat exposes whether a file has acknowledged bytes not yet committed, so the web UI can
// omit the ETag while the overlay would answer (docs/13 §4).
// Invariant: I-69
func TestVFS_StatReportsDirty(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fh, err := fs.OpenFile("/d", os.O_RDWR|os.O_CREATE, 0o644)
	must(t, err)
	_, err = fh.Write(testharness.Pattern(100, 7))
	must(t, err)
	must(t, fh.Close())
	dirty := func(name string) bool {
		t.Helper()
		fi, err := fs.Stat(name)
		must(t, err)
		d, ok := fi.(interface{ Dirty() bool })
		if !ok {
			t.Fatalf("%T has no Dirty()", fi)
		}
		return d.Dirty()
	}
	if !dirty("/d") {
		t.Fatal("unflushed file not reported dirty")
	}
	must(t, f.Flusher().FlushAll(ctx))
	if dirty("/d") {
		t.Fatal("flushed file still reported dirty")
	}
	must(t, fs.MkdirAll("/dir", 0o755))
	if dirty("/dir") {
		t.Fatal("a directory is never dirty")
	}
}
