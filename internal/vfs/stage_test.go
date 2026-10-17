package vfs

import (
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// freeFS reports a fixed free and total size for the staging directory.
type freeFS struct {
	sysfs.FS
	free, total uint64
	err         error
	calls       int
}

func (f *freeFS) Free(string) (uint64, uint64, error) {
	f.calls++
	return f.free, f.total, f.err
}

// Without staging.size the stage is a quarter of the staging filesystem's free bytes,
// what the logs already hold counted as free, never under 1 GiB; WALCap follows it, never
// under 128 MiB; an explicit option wins.
// Invariant: I-396
func TestOverlay_Stage_Derived(t *testing.T) {
	const gib = uint64(1) << 30
	cases := []struct {
		name      string
		free      uint64
		err       error
		opts      []Option
		wantStage uint64
		wantCap   int64
	}{
		{"a quarter of 400 GiB", 400 * gib, nil, nil, 100 * gib, 100 << 30},
		{"floor at 1 GiB", 2 * gib, nil, nil, gib, 1 << 30},
		{"exactly 4 GiB", 4 * gib, nil, nil, gib, 1 << 30},
		{"just over 4 GiB", 4*gib + 4, nil, nil, gib + 1, 1<<30 + 1},
		{"Free fails", 0, errors.New("statfs"), nil, gib, 1 << 30},
		{"staging.size wins", 400 * gib, nil, []Option{WithMaxDirty(3 * gib)}, 3 * gib, 3 << 30},
		{"an explicit WAL cap wins", 400 * gib, nil, []Option{WithWALCap(64 << 20)}, 100 * gib, 64 << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ffs := &freeFS{FS: sysfs.OS{}, free: c.free, total: 1000 * gib, err: c.err}
			ov, _ := newOverlay(t, t.TempDir(), append([]Option{WithFS(ffs)}, c.opts...)...)
			if got := ov.Stage(); got != c.wantStage {
				t.Fatalf("stage %d, want %d", got, c.wantStage)
			}
			if got := ov.WALCap(); got != c.wantCap {
				t.Fatalf("WAL cap %d, want %d", got, c.wantCap)
			}
		})
	}
}

// The logs a restarted gateway replays count as free when the stage is derived: a stage
// that held 40 GiB before the restart is not shrunk by its own bytes.
// Invariant: I-396
func TestOverlay_Stage_CountsOwnLogs(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	must(t, ov.Write(meta.Ino(70), 0, 0, pattern(1<<20, 1)))
	must(t, ov.Close())
	ffs := &freeFS{FS: sysfs.OS{}, free: 8 << 30, total: 100 << 30}
	ov2, _ := newOverlay(t, dir, WithFS(ffs))
	held := uint64(ov2.Dirty()[0].WALBytes)
	if want := (8<<30 + held) / 4; ov2.Stage() != want {
		t.Fatalf("stage %d, want (free + %d held) / 4 = %d", ov2.Stage(), held, want)
	}
}

// The stage is a cap like MaxDirty: {0, 1, max−1, max, max+1} bytes against it.
// Invariant: I-396, I-79
func TestOverlay_Stage_Boundaries(t *testing.T) {
	const stage = 64 << 10
	cases := []struct {
		name    string
		before  int // dirty bytes already held
		write   int
		wantErr bool
	}{
		{"empty", 0, 1, false},
		{"one byte", 1, 1, false},
		{"stage−1", stage - 1, 1, false},
		{"at the stage", stage, 1, true},
		{"stage+1", stage + 1, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ov, _ := newOverlay(t, t.TempDir(), WithMaxDirty(stage), WithWALCap(1<<30), WithCapWait(0))
			if c.before > 0 {
				must(t, ov.Write(meta.Ino(71), 0, 0, pattern(c.before, 1)))
			}
			err := ov.Write(meta.Ino(72), 0, 10, pattern(c.write, 2))
			if (err != nil) != c.wantErr {
				t.Fatalf("write with %d held: %v, want error %v", c.before, err, c.wantErr)
			}
		})
	}
}

// A WRITE waits while the staging filesystem is under its reserve (2 % of its size, at
// least 1 GiB) and proceeds once it is above; the free bytes are read at most once a
// second, however many WRITEs arrive.
// Invariant: I-396
func TestOverlay_StageReserve_Waits(t *testing.T) {
	const gib = uint64(1) << 30
	cases := []struct {
		name    string
		free    uint64
		total   uint64
		wantErr bool
	}{
		{"above 1 GiB on a small disk", gib + 1, 10 * gib, false},
		{"at 1 GiB", gib, 10 * gib, false},
		{"under 1 GiB", gib - 1, 10 * gib, true},
		{"2 % of a large disk", 20 * gib, 1000 * gib, false},
		{"under 2 % of a large disk", 20*gib - 1, 1000 * gib, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ffs := &freeFS{FS: sysfs.OS{}, free: c.free, total: c.total}
			ov, _ := newOverlay(t, t.TempDir(), WithFS(ffs), WithMaxDirty(gib), WithCapWait(0))
			err := ov.Write(meta.Ino(73), 0, 10, []byte("x"))
			if (err != nil) != c.wantErr {
				t.Fatalf("write with %d free of %d: %v, want error %v", c.free, c.total, err, c.wantErr)
			}
			if c.wantErr && !errors.Is(err, ErrDirtyFull) {
				t.Fatalf("refusal %v, want the cap's ErrDirtyFull", err)
			}
		})
	}
	t.Run("read once a second", func(t *testing.T) {
		ffs := &freeFS{FS: sysfs.OS{}, free: 10 * gib, total: 100 * gib}
		ov, clk := newOverlay(t, t.TempDir(), WithFS(ffs), WithMaxDirty(gib), WithCapWait(0))
		ffs.calls = 0
		for i := range 100 {
			must(t, ov.Write(meta.Ino(74), 0, uint64(i), []byte("x")))
		}
		if ffs.calls != 1 {
			t.Fatalf("Free read %d times for 100 WRITEs within a second", ffs.calls)
		}
		ffs.free = 0
		must(t, ov.Write(meta.Ino(74), 0, 200, []byte("x"))) // still the cached reading
		clk.Advance(time.Second)
		if err := ov.Write(meta.Ino(74), 0, 201, []byte("x")); !errors.Is(err, ErrDirtyFull) {
			t.Fatalf("after a second with the disk full: %v, want ErrDirtyFull", err)
		}
	})
}

// A WRITE past the last extent appends in place: a sequential run allocates nothing per
// WRITE once the slice has grown.
// Invariant: I-399
func TestExtents_AppendInPlace(t *testing.T) {
	var e extents
	for i := range 1024 {
		e.insert(extent{off: uint64(i) * 10, n: 10, walOff: int64(i) * 100})
	}
	e = append(make(extents, 0, 4096), e...)
	next := uint64(1024 * 10)
	allocs := testing.AllocsPerRun(1000, func() {
		e.insert(extent{off: next, n: 10, walOff: int64(next) * 10})
		next += 10
	})
	if allocs != 0 {
		t.Fatalf("an append past the end allocated %.1f times", allocs)
	}
	// and the result is still the sorted, non-overlapping set
	for i := 1; i < len(e); i++ {
		if e[i].off < e[i-1].end() {
			t.Fatalf("extents overlap at %d: %+v %+v", i, e[i-1], e[i])
		}
	}
	// an append that touches the last extent is not the fast path: it overrides
	last := e[len(e)-1]
	e.insert(extent{off: last.off + 5, n: 10, walOff: 1})
	if got := e[len(e)-2]; got.off != last.off || got.n != 5 {
		t.Fatalf("overlapping append kept %+v, want the first 5 bytes of %+v", got, last)
	}
}
