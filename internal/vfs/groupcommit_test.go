package vfs

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
)

// waitFor waits for n tokens on entered or fails the test.
func waitFor(t *testing.T, entered <-chan struct{}, n int) {
	t.Helper()
	for range n {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the gated step was never reached")
		}
	}
}

// Concurrent appends share one fsync: with the first append's fsync held, the appends
// that arrive meanwhile are written and wait; when it is released, one more fsync covers
// all of them, and every append succeeds with its record in the log.
// Invariant: I-168, I-7
func TestWAL_GroupCommit_OneFsyncCoversConcurrentAppends(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	w, err := openWAL(ff, filepath.Join(t.TempDir(), "1.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	must(t, w.AppendBase(0))
	syncs := ff.Calls(sysfstest.StepSync)
	entered, release := ff.BlockAt(sysfstest.StepSync)
	const appends = 8
	errs := make(chan error, appends)
	for i := range appends {
		go func() {
			_, err := w.AppendWrite(uint64(i)*4096, []byte("rec"))
			errs <- err
		}()
	}
	waitFor(t, entered, 1) // one append is in its fsync; the rest write and wait
	eventually(t, func() bool { return w.Size() == w.Synced()+appends*(recHeaderLen+3) }, "the other appends never reached the log")
	release()
	for range appends {
		must(t, <-errs)
	}
	if n := ff.Calls(sysfstest.StepSync) - syncs; n > 2 {
		t.Fatalf("%d fsyncs for %d concurrent appends, want at most 2", n, appends)
	}
	if recs := replayAll(t, w); len(recs) != appends+1 {
		t.Fatalf("%d records replayed, want %d", len(recs), appends+1)
	}
}

// A failed group fsync fails every append it covered: their records are cut back, none
// is acknowledged, a later append succeeds on the reopened file and replay shows only
// what was acknowledged.
// Invariant: I-168, I-37, I-101
func TestWAL_GroupCommit_FailedFsyncFailsEveryWaiter(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	w, err := openWAL(ff, filepath.Join(t.TempDir(), "2.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	must(t, w.AppendBase(0))
	entered, release := ff.BlockAt(sysfstest.StepSync)
	const waiters = 3
	errs := make(chan error, waiters+1)
	go func() {
		_, err := w.AppendWrite(0, []byte("first"))
		errs <- err
	}()
	waitFor(t, entered, 1) // the first append's fsync is held
	for i := range waiters {
		go func() {
			_, err := w.AppendWrite(uint64(i+1)*4096, []byte("late"))
			errs <- err
		}()
	}
	eventually(t, func() bool { return w.Size() == w.Synced()+(recHeaderLen+5)+waiters*(recHeaderLen+4) }, "the waiters never reached the log")
	boom := errors.New("disk said no")
	ff.FailAt(sysfstest.StepSync, boom) // the next fsync, the waiters' group, fails
	release()
	ok, failed := 0, 0
	for range waiters + 1 {
		if err := <-errs; err == nil {
			ok++
		} else {
			failed++
		}
	}
	if ok != 1 || failed != waiters {
		t.Fatalf("%d appends acknowledged and %d failed, want 1 and %d", ok, failed, waiters)
	}
	if _, err := w.AppendWrite(99999, []byte("after")); err != nil {
		t.Fatalf("append after the failed group: %v", err)
	}
	recs := replayAll(t, w)
	if len(recs) != 3 || recs[1].off != 0 || recs[2].off != 99999 {
		t.Fatalf("replayed %+v, want base, first, after", recs)
	}
}

// A record written while an fsync runs is not covered by it: its append waits for the
// next fsync and returns only once that one succeeded.
// Invariant: I-168
func TestWAL_GroupCommit_LateAppendWaitsForNextFsync(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	w, err := openWAL(ff, filepath.Join(t.TempDir(), "3.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	must(t, w.AppendBase(0))
	syncs := ff.Calls(sysfstest.StepSync)
	entered, release := ff.BlockAt(sysfstest.StepSync)
	first := make(chan error, 1)
	go func() {
		_, err := w.AppendWrite(0, []byte("a"))
		first <- err
	}()
	waitFor(t, entered, 1)
	late := make(chan error, 1)
	go func() {
		_, err := w.AppendWrite(4096, []byte("b"))
		late <- err
	}()
	eventually(t, func() bool { return w.Size() == w.Synced()+2*(recHeaderLen+1) }, "the late append never reached the log")
	release()
	must(t, <-first)
	must(t, <-late)
	if n := ff.Calls(sysfstest.StepSync) - syncs; n != 2 {
		t.Fatalf("%d fsyncs, want 2: the late record needed its own", n)
	}
	if w.Synced() != w.Size() {
		t.Fatalf("synced %d of %d", w.Synced(), w.Size())
	}
}

// Until its fsync succeeds a WRITE is invisible: a READ meanwhile shows the old bytes
// and the dirty count excludes it; once acknowledged it shows.
// Invariant: I-168, I-34
func TestOverlay_PendingWrite_NotVisibleUntilFsynced(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	ov, _ := newOverlay(t, t.TempDir(), WithFS(ff))
	ino := meta.Ino(61)
	must(t, ov.Write(ino, 0, 0, []byte("old")))
	entered, release := ff.BlockAt(sysfstest.StepSync)
	defer release()
	done := make(chan error, 1)
	go func() { done <- ov.Write(ino, 0, 0, []byte("new")) }()
	waitFor(t, entered, 1)
	if got := readFrom(t, ov, ino, 0, baseOf(nil), 3); string(got) != "old" {
		t.Fatalf("read %q during the fsync, want the old bytes", got)
	}
	if d := ov.Dirty(); len(d) != 1 || d[0].DirtyBytes != 3 {
		t.Fatalf("dirty during the fsync: %+v", d)
	}
	release()
	must(t, <-done)
	if got := readFrom(t, ov, ino, 0, baseOf(nil), 3); string(got) != "new" {
		t.Fatalf("read %q after the ack", got)
	}
}

// A failed group fsync drops every pending WRITE: nothing of them is visible, the dirty
// count excludes them, and a restart replays only the acknowledged bytes.
// Invariant: I-168, I-7, I-37
func TestOverlay_FailedGroupFsync_DropsEveryPendingWrite(t *testing.T) {
	dir := t.TempDir()
	ff := sysfstest.New(sysfs.OS{})
	ov, _ := newOverlay(t, dir, WithFS(ff))
	ino := meta.Ino(62)
	must(t, ov.Write(ino, 0, 4096, []byte("kept")))
	entered, release := ff.BlockAt(sysfstest.StepSync)
	first := make(chan error, 1)
	go func() { first <- ov.Write(ino, 0, 0, []byte("first")) }()
	waitFor(t, entered, 1)
	const waiters = 3
	late := make(chan error, waiters)
	for i := range waiters {
		go func() { late <- ov.Write(ino, 0, uint64(8192+i*100), []byte("late")) }()
	}
	eventually(t, func() bool {
		d, _ := ov.get(ino, 0, false)
		defer d.mu.Unlock()
		return len(d.pending) == waiters+1
	}, "the waiters never reached the log")
	ff.FailAt(sysfstest.StepSync, errors.New("disk said no"))
	release()
	must(t, <-first)
	for range waiters {
		if err := <-late; err == nil {
			t.Fatal("a WRITE whose fsync failed was acknowledged")
		}
	}
	if d := ov.Dirty(); len(d) != 1 || d[0].DirtyBytes != 9 {
		t.Fatalf("dirty after the failed group: %+v", d)
	}
	want := append([]byte("first"), make([]byte, 4096-5)...)
	want = append(want, "kept"...)
	if got := readFrom(t, ov, ino, 0, baseOf(nil), 4100); !bytes.Equal(got, want) {
		t.Fatalf("live view differs at %d", firstDiff(got, want))
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := readFrom(t, ov2, ino, 0, baseOf(nil), 4100); !bytes.Equal(got, want) {
		t.Fatalf("after restart: differs at %d", firstDiff(got, want))
	}
}

// A flush commit (compaction) waits for an append between its write and its fsync: the
// late WRITE is acknowledged, survives the compaction and replays after a restart.
// Invariant: I-168, I-35
func TestOverlay_CommitFlushed_WaitsForPendingAppend(t *testing.T) {
	dir := t.TempDir()
	ff := sysfstest.New(sysfs.OS{})
	ov, _ := newOverlay(t, dir, WithFS(ff))
	ino := meta.Ino(63)
	must(t, ov.Write(ino, 0, 0, []byte("planned")))
	must(t, ov.Write(ino, 0, 100, []byte("residual")))
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("nothing to flush")
	}
	entered, release := ff.BlockAt(sysfstest.StepSync)
	late := make(chan error, 1)
	go func() { late <- ov.Write(ino, 0, 200, []byte("late")) }()
	waitFor(t, entered, 1)
	plan.Residual = plan.Extents[1:] // the first extent was committed, the rest stays
	committed := make(chan error, 1)
	go func() { committed <- ov.CommitFlushed(plan, 7) }()
	select {
	case err := <-committed:
		t.Fatalf("CommitFlushed returned (%v) while a WRITE was mid-fsync", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	must(t, <-late)
	must(t, <-committed)
	want := make([]byte, 204)
	copy(want[100:], "residual")
	copy(want[200:], "late")
	if got := readFrom(t, ov, ino, 7, baseOf([]byte("planned")), 204); !bytes.Equal(got[7:], want[7:]) {
		t.Fatalf("live view differs at %d", firstDiff(got, want))
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := readFrom(t, ov2, ino, 7, baseOf([]byte("planned")), 204); !bytes.Equal(got[7:], want[7:]) {
		t.Fatalf("after restart: differs at %d", firstDiff(got, want))
	}
}

// The inode lock is not held across the fsync: a Size query completes while a WRITE
// to the same inode waits for its fsync.
// Invariant: I-168
func TestOverlay_WriteDoesNotHoldInodeLockDuringFsync(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	ov, _ := newOverlay(t, t.TempDir(), WithFS(ff))
	ino := meta.Ino(64)
	must(t, ov.Write(ino, 0, 0, []byte("x")))
	entered, release := ff.BlockAt(sysfstest.StepSync)
	defer release()
	done := make(chan error, 1)
	go func() { done <- ov.Write(ino, 0, 10, []byte("y")) }()
	waitFor(t, entered, 1)
	sized := make(chan struct{})
	go func() {
		ov.Size(ino, 0)
		close(sized)
	}()
	select {
	case <-sized:
	case <-time.After(5 * time.Second):
		t.Fatal("Size blocked behind the WRITE's fsync")
	}
	release()
	must(t, <-done)
}

// A truncate that lands while a WRITE is mid-fsync applies in log order: the live view
// after both equals the view a restart rebuilds from the log.
// Invariant: I-168, I-37
func TestOverlay_TruncateDuringPendingAppend_MatchesReplay(t *testing.T) {
	dir := t.TempDir()
	ff := sysfstest.New(sysfs.OS{})
	ov, _ := newOverlay(t, dir, WithFS(ff))
	ino := meta.Ino(65)
	must(t, ov.Write(ino, 0, 0, []byte("head")))
	entered, release := ff.BlockAt(sysfstest.StepSync)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- ov.Write(ino, 0, 100, []byte("tail")) }()
	waitFor(t, entered, 1)
	go func() { defer wg.Done(); errs <- ov.Truncate(ino, 0, 50) }()
	eventually(t, func() bool {
		d, _ := ov.get(ino, 0, false)
		defer d.mu.Unlock()
		return len(d.pending) == 2
	}, "the truncate never reached the log")
	release()
	wg.Wait()
	must(t, <-errs)
	must(t, <-errs)
	live := readFrom(t, ov, ino, 0, baseOf(nil), 200)
	size, _ := ov.Size(ino, 0)
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	replayed := readFrom(t, ov2, ino, 0, baseOf(nil), 200)
	size2, _ := ov2.Size(ino, 0)
	if size != 50 || size2 != size || !bytes.Equal(live, replayed) {
		t.Fatalf("live size %d view %q; replayed size %d view %q", size, live[:8], size2, replayed[:8])
	}
}

// Sixteen appenders of 32 KiB against one WAL on a real directory: what group commit
// makes of concurrent WRITEs (compare BenchmarkWAL_Append).
func BenchmarkWAL_ConcurrentAppends(b *testing.B) {
	w, err := openWAL(sysfs.OS{}, filepath.Join(b.TempDir(), "b.wal"))
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	rec := make([]byte, 32<<10)
	b.SetBytes(int64(len(rec)))
	b.SetParallelism(16)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := w.AppendWrite(0, rec); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// One appender of 32 KiB: the per-record fsync cost on this disk.
func BenchmarkWAL_Append(b *testing.B) {
	w, err := openWAL(sysfs.OS{}, filepath.Join(b.TempDir(), "b.wal"))
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	rec := make([]byte, 32<<10)
	b.SetBytes(int64(len(rec)))
	b.ResetTimer()
	for range b.N {
		if _, err := w.AppendWrite(0, rec); err != nil {
			b.Fatal(err)
		}
	}
}
