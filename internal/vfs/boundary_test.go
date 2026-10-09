package vfs

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// A plan's extents cover a read only below the plan's floor. Bytes at or past it may
// have been committed by another gateway while the plan is outstanding, and they live in
// the base: a read across the floor sees them, not zeros.
//
// Invariant: I-36
func TestOverlay_ReadAcrossPlanFloor_SeesBase(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(77)
	must(t, ov.Write(ino, 0, 0, bytes.Repeat([]byte{'d'}, 4096)))
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok || plan.Size != 4096 {
		t.Fatalf("plan %+v %v", plan, ok)
	}
	// another gateway committed 8192 bytes meanwhile: the base holds 'B' past our plan
	base := func(off uint64, p []byte) error {
		for i := range p {
			p[i] = 'B'
		}
		return nil
	}
	for _, c := range []struct {
		name            string
		off, n          uint64
		wantFirst, want byte // the first and the last byte of the read
	}{
		{"below the floor", 0, 4095, 'd', 'd'},
		{"up to the floor", 0, 4096, 'd', 'd'},
		{"one past the floor", 0, 4097, 'd', 'B'},
		{"across the floor", 4000, 200, 'd', 'B'},
		{"from the floor", 4096, 4096, 'B', 'B'},
		{"entirely past the floor", 5000, 100, 'B', 'B'},
	} {
		buf := make([]byte, c.n)
		n, err := ov.Read(ino, 8192, c.off, buf, base)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if uint64(n) != c.n {
			t.Fatalf("%s: read %d bytes, want %d", c.name, n, c.n)
		}
		if buf[0] != c.wantFirst || buf[n-1] != c.want {
			t.Fatalf("%s: first %q last %q, want %q and %q", c.name, buf[0], buf[n-1], c.wantFirst, c.want)
		}
	}
	must(t, ov.CommitFlushed(plan, 8192))
}
