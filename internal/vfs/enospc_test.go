package vfs_test

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// A write that meets the cap behind a flush that failed because every holder was full
// maps to ENOSPC with both sentinels still reachable.
// Invariant: I-79
// Lifecycle: L-189
func TestVFS_ClusterFull_ENOSPC(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	f.RestartOverlay(vfs.WithWALCap(1000))
	for n := meta.NodeIdx(1); n <= 3; n++ {
		f.SetFull(n, true)
	}
	fs := f.FS()
	write := func(off int64, n int) error {
		fh, err := fs.OpenFile("/a", os.O_RDWR|os.O_CREATE, 0o644)
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
	must(t, write(0, 900))
	if err := f.Flusher().FlushAll(ctx); !errors.Is(err, chunk.ErrFull) {
		t.Fatalf("flush against full holders: %v", err)
	}
	must(t, write(0, 900)) // the WAL reaches its cap
	err := write(0, 1)
	if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, vfs.ErrWALFull) || !errors.Is(err, chunk.ErrFull) {
		t.Fatalf("err = %v, want ENOSPC wrapping ErrWALFull and chunk.ErrFull", err)
	}
	if errors.Is(err, syscall.EIO) {
		t.Fatalf("err = %v: an unwritable cluster is not an I/O error", err)
	}
}
