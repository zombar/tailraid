package vfs

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
)

// recordingFS records the largest single write to any compaction temp file.
type recordingFS struct {
	sysfs.FS
	mu       sync.Mutex
	maxWrite int
}

func (r *recordingFS) OpenFile(name string, flag int, perm fs.FileMode) (sysfs.File, error) {
	f, err := r.FS.OpenFile(name, flag, perm)
	if err != nil || !strings.HasSuffix(name, ".wal.new") {
		return f, err
	}
	return &recordingFile{File: f, r: r}, nil
}

type recordingFile struct {
	sysfs.File
	r *recordingFS
}

func (f *recordingFile) WriteAt(p []byte, off int64) (int, error) {
	f.r.mu.Lock()
	f.r.maxWrite = max(f.r.maxWrite, len(p))
	f.r.mu.Unlock()
	return f.File.WriteAt(p, off)
}

// A compaction streams a 5 MiB live record through a bounded buffer: no single write to
// the new log exceeds the buffer, and the compacted log replays to the same bytes.
// Invariant: I-80
// Lifecycle: L-172
func TestWAL_Compact_StreamsThroughBoundedBuffer(t *testing.T) {
	rec := &recordingFS{FS: sysfs.OS{}}
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir, WithFS(rec))
	ov.compactMin = -1 << 30 // compact at every commit
	ino := meta.Ino(51)
	live := pattern(5<<20, 9)
	must(t, ov.Write(ino, 0, 0, pattern(4096, 1))) // flushed by the plan below
	plan, _ := ov.SnapshotForFlush(ino)
	must(t, ov.Write(ino, 0, 4096, live)) // arrives during the flush: residual, compacted
	must(t, ov.CommitFlushed(plan, 4096))
	if rec.maxWrite == 0 || rec.maxWrite > compactBuf {
		t.Fatalf("largest write to the compacted log: %d bytes, want ≤ %d", rec.maxWrite, compactBuf)
	}
	want := append(make([]byte, 4096), live...)
	if got := readFrom(t, ov, ino, 4096, baseOf(make([]byte, 4096)), uint64(len(want))); !bytes.Equal(got, want) {
		t.Fatalf("after compaction differs at %d", firstDiff(got, want))
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := readFrom(t, ov2, ino, 4096, baseOf(make([]byte, 4096)), uint64(len(want))); !bytes.Equal(got, want) {
		t.Fatalf("after replay differs at %d", firstDiff(got, want))
	}
}

// A directory fsync that fails after the rename leaves the WAL on the new, reopened log
// with valid offsets: appends, reads and a replay after reopen all work.
// Invariant: I-37, I-80
func TestWAL_Compact_SyncDirFails_LogStillUsable(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	w, err := openWAL(ff, filepath.Join(t.TempDir(), "7.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	must(t, w.AppendBase(0))
	_, _ = w.AppendWrite(0, bytes.Repeat([]byte("D"), 3000))
	live, _ := w.AppendWrite(500, []byte("live-record"))
	boom := errors.New("dir fsync boom")
	ff.FailAt(sysfstest.StepSyncDir, boom)
	offs, err := w.Compact([]walRec{{typ: recBase, off: 10}, {typ: recWrite, off: 500, src: live, n: 11}})
	if !errors.Is(err, boom) {
		t.Fatalf("Compact err = %v, want the fsync error", err)
	}
	if len(offs) != 2 {
		t.Fatalf("offs = %v: the renamed log's offsets must be returned", offs)
	}
	if got := readWAL(t, w, offs[1], 11); string(got) != "live-record" {
		t.Fatalf("read after the failed step: %q", got)
	}
	if _, err := w.AppendWrite(9000, []byte("after")); err != nil {
		t.Fatalf("append after the failed step: %v", err)
	}
	recs := replayAll(t, w)
	if len(recs) != 3 || recs[0].off != 10 || string(recs[1].data) != "live-record" || string(recs[2].data) != "after" {
		t.Fatalf("replay %+v", recs)
	}
}

// Through the overlay: a compaction whose directory fsync fails reports the error, and the
// residual extent is still readable at its new offset and after a reopen.
// Invariant: I-37, I-80
func TestOverlay_CommitFlushed_PostRenameError_OffsetsValid(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir, WithFS(ff))
	ov.compactMin = -1 << 30 // compact at every commit
	ino := meta.Ino(52)
	must(t, ov.Write(ino, 0, 0, pattern(4096, 1)))
	plan, _ := ov.SnapshotForFlush(ino)
	must(t, ov.Write(ino, 0, 4096, []byte("residual")))
	boom := errors.New("dir fsync boom")
	ff.FailAt(sysfstest.StepSyncDir, boom)
	if err := ov.CommitFlushed(plan, 4096); !errors.Is(err, boom) {
		t.Fatalf("CommitFlushed err = %v, want the fsync error", err)
	}
	want := append(make([]byte, 4096), "residual"...)
	if got := readFrom(t, ov, ino, 4096, baseOf(make([]byte, 4096)), uint64(len(want))); !bytes.Equal(got, want) {
		t.Fatalf("after the failed compaction differs at %d", firstDiff(got, want))
	}
	must(t, ov.Write(ino, 4096, 5000, []byte("more")))
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir, WithFS(ff))
	buf := make([]byte, 5004)
	copy(buf, want)
	copy(buf[5000:], "more")
	if got := readFrom(t, ov2, ino, 4096, baseOf(make([]byte, 4096)), 5004); !bytes.Equal(got, buf) {
		t.Fatalf("after replay differs at %d", firstDiff(got, buf))
	}
}
