package vfs_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
)

// A proposal whose outcome is unknown was applied by the leader, but the gateway never
// heard. Every mutation resolves it instead of retrying blindly: a create or mkdir opens
// what is there, a symlink accepts the link it made, a rename and a remove take "already
// gone" as their own first attempt, chmod repeats. Repeated unknown outcomes are proposed
// again, each read the same way, until the gateway is stale; then EIO.
//
// Invariant: I-95
func TestVFS_UnknownOutcome_ResolvesPerCommand(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()

	f.ProposeUnknown(1)
	must(t, fs.MkdirAll("/d", 0o755))
	if st, err := fs.Stat("/d"); err != nil || !st.IsDir() {
		t.Fatalf("mkdir after an unknown outcome: %v", err)
	}

	f.ProposeUnknown(1)
	file, err := fs.Create("/d/f")
	must(t, err)
	must(t, file.Close())
	if _, err := fs.Stat("/d/f"); err != nil {
		t.Fatalf("create after an unknown outcome: %v", err)
	}

	f.ProposeUnknown(1)
	must(t, fs.Symlink("/d/f", "/d/l"))
	if target, err := fs.Readlink("/d/l"); err != nil || target != "/d/f" {
		t.Fatalf("symlink after an unknown outcome: %q %v", target, err)
	}
	// a link that exists with another target is still EEXIST, unknown outcome or not
	f.ProposeUnknown(1)
	if err := fs.Symlink("/elsewhere", "/d/l"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("symlink over a different link: %v, want EEXIST", err)
	}

	f.ProposeUnknown(1)
	must(t, fs.Chmod("/d/f", 0o600))
	if st, _ := fs.Stat("/d/f"); st.Mode().Perm() != 0o600 {
		t.Fatalf("chmod after an unknown outcome: %v", st.Mode())
	}

	f.ProposeUnknown(1)
	must(t, fs.Rename("/d/f", "/d/g"))
	if _, err := fs.Stat("/d/f"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source after rename: %v", err)
	}
	if _, err := fs.Stat("/d/g"); err != nil {
		t.Fatalf("target after rename: %v", err)
	}

	f.ProposeUnknown(1)
	must(t, fs.Remove("/d/g"))
	if _, err := fs.Stat("/d/g"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file after remove: %v", err)
	}
	must(t, fs.Remove("/d/l"))
	f.ProposeUnknown(1)
	must(t, fs.Remove("/d"))
	if _, err := fs.Stat("/d"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory after rmdir: %v", err)
	}

	// a remove of something that really is not there stays ENOENT
	if err := fs.Remove("/d"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove of a missing entry: %v", err)
	}

	// three unknown outcomes in a row, as a leader killed mid-failover leaves them: the
	// repeats back off on the clock and the mkdir resolves
	f.ProposeUnknown(3)
	done := make(chan error, 1)
	go func() { done <- fs.MkdirAll("/e", 0o755) }()
	testharness.Eventually(t, func() bool {
		f.Clk.Advance(time.Second)
		select {
		case err := <-done:
			must(t, err)
			return true
		default:
			return false
		}
	}, "mkdir never resolved its unknown outcomes")
	if st, err := fs.Stat("/e"); err != nil || !st.IsDir() {
		t.Fatalf("mkdir after three unknown outcomes: %v", err)
	}

	// unknown outcomes while the gateway is stale: EIO, the outcome never resolved
	f.SetStale(true)
	f.ProposeUnknown(2)
	if err := fs.MkdirAll("/g", 0o755); !errors.Is(err, syscall.EIO) {
		t.Fatalf("unknown outcomes on a stale gateway: %v, want EIO", err)
	}
	f.SetStale(false)
}
