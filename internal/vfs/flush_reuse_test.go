package vfs_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
)

// A retry soon after a failed Commit reuses the shards the first attempt placed: it
// checks each slot is still held (HEAD) and PUTs nothing, so a flush that keeps failing
// on a slow link does not push the whole file over that link again every attempt (on the
// real cluster each retry of a 44 MB flush re-PUT 73 MB and saturated the uplink).
// Invariant: I-312
// Lifecycle: L-58
func TestFlusher_SoonRetry_ReusesPlacedShards(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(3*cs, 23)
	must(t, f.Ov.Write(ino, 0, 8192, data))
	fl := f.Flusher()
	f.ProposeErrOnce(errors.New("leadership lost"))
	if err := fl.FlushInode(ctx, ino); err == nil {
		t.Fatal("expected the proposal to fail")
	}
	first := f.Puts()
	if first == 0 {
		t.Fatal("the first attempt PUT nothing")
	}
	f.Clk.Advance(time.Minute) // well inside grace/2
	must(t, fl.FlushInode(ctx, ino))
	if n := f.Puts() - first; n != 0 {
		t.Fatalf("retry PUT %d shards, want 0 (first attempt %d)", n, first)
	}
	want := append(make([]byte, 8192), data...)
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("differs at %d", testharness.FirstDiff(got, want))
	}
}
