package vfs_test

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
)

// A copy sets the source's times right after its last write (cp -p, rsync -a, robocopy,
// Explorer): those times survive the flush that commits the data, every flush of a file
// flushed in parts; a write after them is newer and the flush stamps its own time; and a
// time outside what the volume represents is held at the nearest end, never wrapped.
// Invariant: I-230
func TestVFS_UserTimes_SurviveFlush(t *testing.T) {
	cs := int(testharness.ChunkSize)
	src := time.Date(2019, 3, 14, 15, 9, 26, 0, time.UTC)
	minT, maxT := time.Unix(0, math.MinInt64), time.Unix(0, math.MaxInt64)
	cases := []struct {
		name      string
		set       time.Time
		writeLate bool // write again after setting the times
		want      func(t *testing.T, got time.Time)
	}{
		{"set after the last write", src, false, func(t *testing.T, got time.Time) {
			if !got.Equal(src) {
				t.Fatalf("mtime %v, want the client's %v", got, src)
			}
		}},
		{"a write after the times", src, true, func(t *testing.T, got time.Time) {
			if got.Equal(src) {
				t.Fatal("mtime kept the client's time over a later write")
			}
		}},
		{"before 1678 (a FILETIME of 1601)", time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC), false, func(t *testing.T, got time.Time) {
			if !got.Equal(minT) {
				t.Fatalf("mtime %v, want the earliest representable %v", got, minT)
			}
		}},
		{"after 2262", time.Date(2500, 6, 1, 0, 0, 0, 0, time.UTC), false, func(t *testing.T, got time.Time) {
			if !got.Equal(maxT) {
				t.Fatalf("mtime %v, want the latest representable %v", got, maxT)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			fs := f.FS()
			fh, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE, 0o644)
			must(t, err)
			wa := fh.(interface {
				WriteAt([]byte, int64) (int, error)
			})
			_, err = wa.WriteAt(testharness.Pattern(2*cs+5, 3), 7) // two chunks and a bit, at a non-zero offset
			must(t, err)
			must(t, fs.Chtimes("/f", tc.set, tc.set))
			if tc.writeLate {
				_, err = wa.WriteAt([]byte("late"), 3)
				must(t, err)
			}
			must(t, f.Flusher().FlushAll(ctx))
			fi, err := fs.Stat("/f")
			must(t, err)
			tc.want(t, fi.ModTime())
		})
	}
}
