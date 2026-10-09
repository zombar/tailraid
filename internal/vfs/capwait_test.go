package vfs

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// eventually polls cond on the wall clock (testharness.Eventually, which this internal
// package cannot import).
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout: %s", msg)
		case <-tick.C:
		}
	}
}

// fillToCap writes records until the inode's WAL is at its cap.
func fillToCap(t *testing.T, ov *Overlay, ino meta.Ino) {
	t.Helper()
	for i := 0; ; i++ {
		d := ov.Dirty()
		if len(d) == 1 && d[0].WALBytes >= ov.WALCap() {
			return
		}
		if i > 100 {
			t.Fatal("WAL never reached its cap")
		}
		must(t, ov.Write(ino, 0, 100, bytes.Repeat([]byte("w"), 900)))
	}
}

// A WRITE at the WAL cap waits: it returns, acknowledged, once the flush has freed room,
// which with the default CapWait on a fake clock is the only way it can return at all.
// Invariant: I-79
// Lifecycle: L-188
func TestOverlay_WALCap_WaitsForFlush(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir(), WithWALCap(1000))
	ino := meta.Ino(41)
	fillToCap(t, ov, ino)
	done := make(chan error, 1)
	go func() { done <- ov.Write(ino, 0, 5000, []byte("late")) }()
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("no plan")
	}
	must(t, ov.CommitFlushed(plan, 1000))
	must(t, <-done)
	buf := make([]byte, 4)
	if _, err := ov.Read(ino, 1000, 5000, buf, baseOf(nil)); err != nil || string(buf) != "late" {
		t.Fatalf("read after the wait: %q, %v", buf, err)
	}
}

// The wait is bounded: once CapWait has elapsed on the clock with no room freed, the
// WRITE fails with the cap error.
// Invariant: I-79
func TestOverlay_WALCap_FailsAfterCapWait(t *testing.T) {
	ov, clk := newOverlay(t, t.TempDir(), WithWALCap(1000), WithCapWait(5*time.Second))
	ino := meta.Ino(42)
	fillToCap(t, ov, ino)
	done := make(chan error, 1)
	go func() { done <- ov.Write(ino, 0, 5000, []byte("late")) }()
	var err error
	eventually(t, func() bool {
		clk.Advance(time.Second)
		select {
		case err = <-done:
			return true
		default:
			return false
		}
	}, "write past CapWait")
	if !errors.Is(err, ErrWALFull) {
		t.Fatalf("err = %v, want ErrWALFull", err)
	}
}

// Behind a flush that failed, a WRITE at the cap does not wait: it fails at once with the
// cap error wrapping the flush's.
// Lifecycle: L-189
func TestOverlay_WALCap_FailsFastWhenFlushFailed(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir(), WithWALCap(1000))
	ino := meta.Ino(43)
	fillToCap(t, ov, ino)
	flushErr := errors.New("too few acks")
	ov.NoteFlush(ino, flushErr)
	err := ov.Write(ino, 0, 5000, []byte("late"))
	if !errors.Is(err, ErrWALFull) || !errors.Is(err, flushErr) {
		t.Fatalf("err = %v, want ErrWALFull wrapping the flush error", err)
	}
	if d := ov.Dirty(); len(d) != 1 || d[0].LastErr != flushErr.Error() {
		t.Fatalf("dirty info %+v", d)
	}
	ov.NoteFlush(ino, nil) // a later success clears it
	if d := ov.Dirty(); d[0].LastErr != "" {
		t.Fatalf("flush error not cleared: %+v", d)
	}
}

// The gateway-wide dirty count includes the bytes of outstanding plans, at the
// boundaries of MaxDirty: with n bytes in a plan and nothing fresh, a one-byte write to
// another inode is accepted iff n < MaxDirty.
// Invariant: I-80
func TestOverlay_TotalDirty_CountsPlanBytes(t *testing.T) {
	const maxDirty = 3000
	for _, c := range []struct {
		name string
		n    int
		ok   bool
	}{{"1", 1, true}, {"max-1", maxDirty - 1, true}, {"max", maxDirty, false}, {"max+1", maxDirty + 1, false}} {
		t.Run(c.name, func(t *testing.T) {
			ov, _ := newOverlay(t, t.TempDir(), WithMaxDirty(maxDirty), WithCapWait(0))
			a, b := meta.Ino(44), meta.Ino(45)
			must(t, ov.Write(a, 0, 0, bytes.Repeat([]byte("p"), c.n)))
			plan, ok := ov.SnapshotForFlush(a)
			if !ok {
				t.Fatal("no plan")
			}
			if got := ov.TotalDirty(); got != uint64(c.n) {
				t.Fatalf("TotalDirty with the plan outstanding = %d, want %d", got, c.n)
			}
			err := ov.Write(b, 0, 0, []byte("x"))
			if c.ok && err != nil {
				t.Fatalf("write with %d plan bytes: %v", c.n, err)
			}
			if !c.ok && !errors.Is(err, ErrDirtyFull) {
				t.Fatalf("write with %d plan bytes: err=%v, want ErrDirtyFull", c.n, err)
			}
			must(t, ov.CommitFlushed(plan, uint64(c.n)))
			want := uint64(0)
			if c.ok {
				want = 1
			}
			if got := ov.TotalDirty(); got != want {
				t.Fatalf("TotalDirty after the commit = %d, want %d", got, want)
			}
		})
	}
}

// An empty plan-free overlay counts zero; a plan aborted back into the fresh set and a
// dropped inode are both reflected in the total.
// Invariant: I-80
func TestOverlay_TotalDirty_FollowsAbortAndDrop(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(46)
	if ov.TotalDirty() != 0 {
		t.Fatal("non-zero at start")
	}
	must(t, ov.Write(ino, 0, 0, bytes.Repeat([]byte("p"), 500)))
	plan, _ := ov.SnapshotForFlush(ino)
	must(t, ov.Write(ino, 0, 500, bytes.Repeat([]byte("q"), 200)))
	if got := ov.TotalDirty(); got != 700 {
		t.Fatalf("fresh+plan = %d, want 700", got)
	}
	ov.AbortFlush(plan)
	if got := ov.TotalDirty(); got != 700 {
		t.Fatalf("after abort = %d, want 700", got)
	}
	ov.Drop(ino)
	if got := ov.TotalDirty(); got != 0 {
		t.Fatalf("after drop = %d, want 0", got)
	}
}
