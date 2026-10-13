package vfs_test

import (
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
)

// A file with unflushed writes reports the time of its last write as its mtime and ctime
// (the time a client set after that write, when one did), as its size already comes from
// the overlay; the flush then stamps its own time, no earlier. Before, a write left the
// committed mtime in place until the flush, about 2 s later: make, rsync and an NFS
// client's cache saw a file of the new size with the old time.
// Invariant: I-261
func TestVFS_DirtyFile_ReportsItsWriteTime(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	write := func(off int64) {
		t.Helper()
		fh, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE, 0o644)
		must(t, err)
		_, err = fh.(interface {
			WriteAt([]byte, int64) (int, error)
		}).WriteAt(testharness.Pattern(100, 5), off)
		must(t, err)
		must(t, fh.Close())
	}
	write(3)
	must(t, f.Flusher().FlushAll(ctx))
	st, err := fs.Stat("/f")
	must(t, err)
	committed := st.ModTime()

	f.Clk.Advance(10 * time.Second)
	wrote := f.Clk.Now()
	write(1000) // a non-zero offset, past the end
	st, err = fs.Stat("/f")
	must(t, err)
	if !st.ModTime().After(committed) || st.ModTime().Before(wrote) {
		t.Fatalf("a dirty file's mtime %v, want its write's %v (committed %v)", st.ModTime(), wrote, committed)
	}
	if st.Size() != 1100 {
		t.Fatalf("dirty size %d, want 1100", st.Size())
	}
	tm := st.(interface {
		Times() (atime, mtime, ctime time.Time)
	})
	if _, m, c := tm.Times(); !m.Equal(st.ModTime()) || c.Before(wrote) {
		t.Fatalf("the SMB times of a dirty file: mtime %v ctime %v, want the write's %v", m, c, wrote)
	}

	// a time the client set after its last write is the one reported (I-230)
	src := time.Date(2019, 3, 14, 15, 9, 26, 0, time.UTC)
	must(t, fs.Chtimes("/f", src, src))
	st, err = fs.Stat("/f")
	must(t, err)
	if !st.ModTime().Equal(src) {
		t.Fatalf("after Chtimes a dirty file reports %v, want the client's %v", st.ModTime(), src)
	}

	// and the flush lands no earlier than the write
	write(2000)
	must(t, f.Flusher().FlushAll(ctx))
	st, err = fs.Stat("/f")
	must(t, err)
	if st.ModTime().Before(wrote) {
		t.Fatalf("after the flush mtime %v went back before the write %v", st.ModTime(), wrote)
	}
}
