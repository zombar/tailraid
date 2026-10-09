package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
)

// A thousand levels resolve, create and read back; a path at exactly MaxPathLen bytes is
// created by every operation and one byte more is ENAMETOOLONG.
//
// Invariant: I-115
func TestVFS_DeepPath_ThousandLevelsAndLimit(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	deep := strings.Repeat("/a", 1000)
	must(t, fs.MkdirAll(deep, 0o755))
	fh, err := fs.Create(deep + "/f")
	must(t, err)
	_, err = fh.Write([]byte("deep"))
	must(t, err)
	must(t, fh.Close())
	st, err := fs.Stat(deep + "/f")
	must(t, err)
	if st.Size() != 4 {
		t.Fatalf("size %d", st.Size())
	}
	// "/a"×2047 is 4094 bytes: "/a" more is 4096 (the limit), "/ab" is 4097
	edge := strings.Repeat("/a", 2047)
	must(t, fs.MkdirAll(edge, 0o755))
	fh, err = fs.Create(edge + "/a")
	must(t, err)
	must(t, fh.Close())
	if _, err := fs.Create(edge + "/ab"); !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("create at 4097 bytes: %v, want ENAMETOOLONG", err)
	}
	if err := fs.MkdirAll(edge+"/bb", 0o755); !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("mkdir at 4097 bytes: %v, want ENAMETOOLONG", err)
	}
	must(t, fs.MkdirAll(edge+"/b", 0o755))
	must(t, fs.Symlink("/x", edge+"/c"))
	if err := fs.Symlink("/x", edge+"/cc"); !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("symlink at 4097 bytes: %v, want ENAMETOOLONG", err)
	}
	if err := fs.Rename(edge+"/a", edge+"/dd"); !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("rename to 4097 bytes: %v, want ENAMETOOLONG", err)
	}
	must(t, fs.Rename(edge+"/a", edge+"/d"))
	if _, err := fs.Stat(edge + "/d"); err != nil {
		t.Fatalf("renamed file at the limit: %v", err)
	}
}

// Eight goroutines write disjoint ranges of one file through their own handles on one
// gateway, straddling chunk boundaries; after the flush every byte is where its writer
// put it and no writer's bytes were lost to another's.
//
// Invariant: I-7, I-45
func TestVFS_ConcurrentWriters_OneInode(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	const writers, block, rounds = 8, 1024, 16
	cs := int(testharness.ChunkSize)
	fh, err := fs.Create("/shared")
	must(t, err)
	must(t, fh.Close())
	offset := func(w, r int) int { return cs/2 + (r*writers+w)*block } // writers interleave within each round
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Go(func() {
			h, err := fs.OpenFile("/shared", os.O_RDWR, 0)
			if err != nil {
				errs <- err
				return
			}
			defer h.Close()
			for r := range rounds {
				if _, err := h.Seek(int64(offset(w, r)), io.SeekStart); err != nil {
					errs <- err
					return
				}
				if _, err := h.Write(testharness.Pattern(block, byte(1+w))); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	want := make([]byte, offset(writers-1, rounds-1)+block)
	for w := range writers {
		for r := range rounds {
			copy(want[offset(w, r):], testharness.Pattern(block, byte(1+w)))
		}
	}
	must(t, f.Flusher().FlushAll(context.Background()))
	h, err := fs.Open("/shared")
	must(t, err)
	got, err := io.ReadAll(h)
	must(t, err)
	must(t, h.Close())
	if !bytes.Equal(got, want) {
		t.Fatalf("first difference at %d (len %d, want %d)", testharness.FirstDiff(got, want), len(got), len(want))
	}
}

// A dangling symlink and a two-link loop are stored and read back as symlinks; opening
// either is ELOOP at once (the gateway never follows a link, the client does), and
// nothing hangs.
func TestVFS_Symlink_DanglingAndLoop(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	must(t, fs.Symlink("/nowhere", "/dangling"))
	if tgt, err := fs.Readlink("/dangling"); err != nil || tgt != "/nowhere" {
		t.Fatalf("readlink: %q %v", tgt, err)
	}
	if _, err := fs.Lstat("/dangling"); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if _, err := fs.Open("/dangling"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("open dangling: %v, want ELOOP", err)
	}
	must(t, fs.Symlink("/l2", "/l1"))
	must(t, fs.Symlink("/l1", "/l2"))
	done := make(chan error, 1)
	go func() {
		_, err := fs.Open("/l1")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("open loop: %v, want ELOOP", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("opening a symlink loop hung")
	}
	must(t, fs.Remove("/l1"))
	if tgt, err := fs.Readlink("/l2"); err != nil || tgt != "/l1" {
		t.Fatalf("readlink after removing the other end: %q %v", tgt, err)
	}
}
