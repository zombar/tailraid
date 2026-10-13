package vfs_test

import (
	"errors"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
)

type validDataFile interface {
	ValidData() (valid, size int64, err error)
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Truncate(size int64) error
}

// checkValid asserts V's promises: lo ≤ V ≤ hi, V ≤ size, and every byte at or past V
// reads as zero.
func checkValid(t *testing.T, phase string, fh validDataFile, lo, hi int64) {
	t.Helper()
	v, size, err := fh.ValidData()
	if err != nil {
		t.Fatalf("%s: ValidData: %v", phase, err)
	}
	if v < lo || v > hi || v > size {
		t.Fatalf("%s: valid %d, size %d; want valid in [%d, %d] and ≤ size", phase, v, size, lo, hi)
	}
	if size > v {
		tail := make([]byte, size-v)
		if _, err := fh.ReadAt(tail, v); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("%s: read past valid: %v", phase, err)
		}
		for i, b := range tail {
			if b != 0 {
				t.Fatalf("%s: byte %d (past valid %d) reads %#x", phase, v+int64(i), v, b)
			}
		}
	}
}

// The valid data length is the end of what was written, before and after the flush, and
// never short of a byte that reads as anything but zero: a file created and sized
// without a write has none (what Windows' copy engine asks of every destination).
// Invariant: I-227
func TestVFS_ValidDataLength(t *testing.T) {
	cs := int64(testharness.ChunkSize)
	type step func(t *testing.T, fh validDataFile)
	write := func(off int64, n int) step {
		return func(t *testing.T, fh validDataFile) {
			if _, err := fh.WriteAt(testharness.Pattern(n, 7), off); err != nil {
				t.Fatal(err)
			}
		}
	}
	trunc := func(n int64) step {
		return func(t *testing.T, fh validDataFile) {
			if err := fh.Truncate(n); err != nil {
				t.Fatal(err)
			}
		}
	}
	type vdlCase struct {
		name       string
		steps      []step
		dirtyLo    int64 // valid while dirty: [dirtyLo, dirtyHi]
		dirtyHi    int64
		flushedLo  int64 // and after the flush
		flushedHi  int64
		flushFirst []step // applied and flushed before steps
	}
	cases := []vdlCase{
		{"created and sized, never written", []step{trunc(5*cs + 7)}, 0, 0, 0, 0, nil},
		{
			"one write at a non-zero offset across a chunk boundary",
			[]step{trunc(4 * cs), write(cs-10, 100)},
			cs + 90, cs + 90, cs + 90, 2 * cs, nil,
		},
		{
			"a write then a truncate below it",
			[]step{write(2*cs+5, 10), trunc(cs + 3)},
			0, 0, 0, cs + 3, nil,
		},
		{
			"flushed data cut by a truncate, then extended",
			[]step{trunc(50), trunc(3 * cs)},
			50, 50, 50, cs,
			[]step{write(0, 100)},
		},
		{
			"a write past flushed data",
			[]step{write(3*cs+1, 1)},
			3*cs + 2, 3*cs + 2, 3*cs + 2, 4 * cs,
			[]step{write(5, 10)},
		},
	}
	for _, n := range []int{0, 1, int(cs) - 1, int(cs), int(cs) + 1} {
		end := 3 + int64(n)
		if n == 0 {
			end = 0 // a write of nothing writes nothing
		}
		cases = append(cases, vdlCase{"write of " + strconv.Itoa(n) + " at 3", []step{write(3, n)}, end, end, end, (end + cs - 1) / cs * cs, nil})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			fs := f.FS()
			bf, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE, 0o644)
			must(t, err)
			fh, ok := bf.(validDataFile)
			if !ok {
				t.Fatal("vfs.File has no ValidData")
			}
			for _, s := range tc.flushFirst {
				s(t, fh)
			}
			if tc.flushFirst != nil {
				must(t, f.Flusher().FlushAll(ctx))
			}
			for _, s := range tc.steps {
				s(t, fh)
			}
			checkValid(t, "dirty", fh, tc.dirtyLo, tc.dirtyHi)
			must(t, f.Flusher().FlushAll(ctx))
			checkValid(t, "flushed", fh, tc.flushedLo, tc.flushedHi)
		})
	}
}
