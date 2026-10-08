package vfs

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// A base fetch (a chunk read that may cross the network) never holds the inode lock:
// a WRITE to the same inode completes while the fetch is blocked, and the read that
// follows shows that write on top of the base bytes.
func TestOverlay_ReadBaseOutsideInodeLock(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	base := pattern(4000, 9)
	var ino meta.Ino
	release, entered := make(chan struct{}), make(chan struct{})
	blockingBase := func(off uint64, p []byte) error {
		close(entered)
		<-release
		copy(p, base[off:])
		return nil
	}
	for i, c := range []struct {
		name string
		read func(p []byte) error
	}{
		{"Read", func(p []byte) error { _, err := ov.Read(ino, 4000, 0, p, blockingBase); return err }},
		{"ReadPlanRange", func(p []byte) error {
			plan, ok := ov.SnapshotForFlush(ino)
			if !ok {
				t.Fatal("nothing to flush")
			}
			defer ov.AbortFlush(plan)
			return ov.ReadPlanRange(plan, 0, p, blockingBase)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ino = meta.Ino(7 + i) // a fresh inode per case
			must(t, ov.Write(ino, 4000, 100, []byte("first")))
			release, entered = make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			defer releaseOnce() // a failing case must not leave the fetch (and the lock) stuck
			got := make([]byte, 400)
			readDone := make(chan error, 1)
			go func() { readDone <- c.read(got) }()
			<-entered
			writeDone := make(chan error, 1)
			go func() { writeDone <- ov.Write(ino, 4000, 200, []byte("second")) }()
			select {
			case err := <-writeDone:
				must(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("WRITE blocked behind the base fetch")
			}
			releaseOnce()
			must(t, <-readDone)
			want := append([]byte(nil), base[:400]...)
			copy(want[100:], "first")
			if c.name == "Read" { // the write that landed during the fetch is visible
				copy(want[200:], "second")
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("read differs at %d", firstDiff(got, want))
			}
		})
	}
}
