package vfs

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
)

// After a failed fsync the WAL carries on with a fresh file handle: the next append is
// durable in its own right, and replay sees every acknowledged record and not the one
// whose fsync failed.
//
// Invariant: I-101
func TestWAL_FsyncFailureReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "3.wal")
	ff := sysfstest.New(sysfs.OS{})
	w, err := openWAL(ff, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendWrite(0, []byte("one")); err != nil {
		t.Fatal(err)
	}
	ff.FailAt(sysfstest.StepSync, errors.New("disk said no"))
	if _, err := w.AppendWrite(10, []byte("lost")); err == nil {
		t.Fatal("an append whose fsync failed was acknowledged")
	}
	if _, err := w.AppendWrite(20, []byte("three")); err != nil {
		t.Fatalf("append after a failed fsync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w2, err := openWAL(sysfs.OS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	var offs []uint64
	err = w2.Replay(func(_ uint8, off uint64, _ uint32, _ int64, _ []byte) error {
		offs = append(offs, off)
		return nil
	})
	if err != nil || len(offs) != 2 || offs[0] != 0 || offs[1] != 20 {
		t.Fatalf("replayed offsets %v (%v), want [0 20]", offs, err)
	}
}

// A compaction whose new log cannot be reopened leaves the WAL unusable rather than
// holding a closed handle: every call answers ErrWALClosed, and the log on disk is
// intact for the restart that replays it.
//
// Invariant: I-101
func TestWAL_CompactReopenFailureClosesLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "4.wal")
	ff := sysfstest.New(sysfs.OS{})
	w, err := openWAL(ff, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AppendBase(0); err != nil {
		t.Fatal(err)
	}
	off, err := w.AppendWrite(0, []byte("keep me"))
	if err != nil {
		t.Fatal(err)
	}
	ff.FailAtNth(sysfstest.StepOpenFile, 2, errors.New("too many open files")) // the first open is the new log; the second is its reopen
	_, err = w.Compact([]walRec{{typ: recBase, off: 0}, {typ: recWrite, off: 0, src: off, n: 7}})
	if !errors.Is(err, ErrWALClosed) {
		t.Fatalf("compact with a failed reopen: %v, want ErrWALClosed", err)
	}
	if _, err := w.AppendWrite(7, []byte("x")); !errors.Is(err, ErrWALClosed) {
		t.Fatalf("append on a closed log: %v", err)
	}
	if err := w.ReadAt(make([]byte, 1), off); !errors.Is(err, ErrWALClosed) {
		t.Fatalf("read on a closed log: %v", err)
	}
	// the compacted log is on disk and replays on restart
	w2, err := openWAL(sysfs.OS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	var got []byte
	err = w2.Replay(func(typ uint8, _ uint64, n uint32, walOff int64, _ []byte) error {
		if typ == recWrite {
			got = make([]byte, n)
			return w2.ReadAt(got, walOff)
		}
		return nil
	})
	if err != nil || string(got) != "keep me" {
		t.Fatalf("replay after the failed reopen: %q %v", got, err)
	}
}
