package vfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
	"github.com/zombar/tailraid/internal/testseed"
)

// segFiles lists the inode's segment files in dir (names only), oldest first.
func segFiles(t *testing.T, dir string, ino meta.Ino) []string {
	t.Helper()
	ns, err := segmentsOf(sysfs.OS{}, filepath.Join(dir, walName(ino)))
	if err != nil {
		t.Fatal(err)
	}
	w := &WAL{path: filepath.Join(dir, walName(ino))}
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, filepath.Base(w.segPath(n)))
	}
	return out
}

// logBytes is what the inode's segments hold on disk.
func logBytes(t *testing.T, dir string, ino meta.Ino) int64 {
	t.Helper()
	var n int64
	for _, name := range segFiles(t, dir, ino) {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		n += st.Size()
	}
	return n
}

// A committed partial flush appends a clean record: the overlay keeps the residual and
// the fresh writes, and a restart replays exactly that state, the committed bytes coming
// from the base again (and the clean record holding the truncate state of the commit).
// Invariant: I-397, I-37
func TestWAL_Clean_ReplaysToOverlay(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(61)
	cs := uint64(testCS)
	first := pattern(int(2*cs+100), 3) // chunks 0 and 1 whole, 100 bytes of chunk 2
	must(t, ov.Write(ino, 0, 0, first))
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("no plan")
	}
	fresh := pattern(50, 9)
	must(t, ov.Write(ino, 0, cs+10, fresh)) // arrives during the flush: over a committed chunk
	commitChunks(plan, []uint64{0, 1}, cs)
	before := logBytes(t, dir, ino)
	must(t, ov.CommitFlushed(plan, 2*cs))
	if after := logBytes(t, dir, ino); after < before {
		t.Fatalf("log shrank from %d to %d: a small log was copied, not cleaned", before, after)
	}
	base := first[:2*cs]
	want := bytes.Clone(first)
	copy(want[cs+10:], fresh)
	if got := readFrom(t, ov, ino, 2*cs, baseOf(base), uint64(len(want))); !bytes.Equal(got, want) {
		t.Fatalf("live view differs at %d", firstDiff(got, want))
	}
	d := ov.Dirty()
	if len(d) != 1 || d[0].DirtyBytes != 100+50 {
		t.Fatalf("dirty after the commit: %+v, want 150 bytes (the residual tail and the fresh write)", d)
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := readFrom(t, ov2, ino, 2*cs, baseOf(base), uint64(len(want))); !bytes.Equal(got, want) {
		t.Fatalf("after replay differs at %d", firstDiff(got, want))
	}
	if d2 := ov2.Dirty(); len(d2) != 1 || d2[0].DirtyBytes != d[0].DirtyBytes {
		t.Fatalf("dirty after replay: %+v, want %+v", d2, d)
	}
}

// commitChunks makes a plan a partial flush of the chunks idxs, as buildCommit does.
func commitChunks(plan *FlushPlan, idxs []uint64, cs uint64) {
	plan.Residual = plan.Extents.without(idxs, cs)
}

// A file written and flushed front to back sheds whole segments: after each commit the
// log holds at most the dirty bytes plus two segments, the oldest segments go first and
// no byte of the log is ever copied.
// Invariant: I-397
func TestWAL_Sequential_DropsSegments(t *testing.T) {
	rec := &recordingFS{FS: sysfs.OS{}}
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir, WithFS(rec), WithSegmentSize(16<<10))
	ino := meta.Ino(62)
	cs := uint64(testCS)
	var off uint64
	var flushed uint64
	for round := range 40 {
		for range 3 { // 3 chunks a round, in 1 KiB writes at an offset
			for range int(cs / 1024) {
				must(t, ov.Write(ino, flushed, off, pattern(1024, byte(round))))
				off += 1024
			}
		}
		plan, ok := ov.SnapshotForFlush(ino)
		if !ok {
			t.Fatal("no plan")
		}
		n := (off - flushed) / cs
		idxs := make([]uint64, 0, n)
		for i := flushed / cs; i < flushed/cs+n-1; i++ { // leave the newest chunk dirty
			idxs = append(idxs, i)
		}
		commitChunks(plan, idxs, cs)
		flushed += uint64(len(idxs)) * cs
		must(t, ov.CommitFlushed(plan, flushed))
		var dirty uint64
		for _, d := range ov.Dirty() {
			dirty += d.DirtyBytes
		}
		if lb := logBytes(t, dir, ino); lb > int64(dirty)+2*(16<<10)+4096 {
			t.Fatalf("round %d: log %d bytes for %d dirty", round, lb, dirty)
		}
	}
	if segs := segFiles(t, dir, ino); len(segs) == 0 || segs[0] == walName(ino) {
		t.Fatalf("segments %v: segment 0 should have been dropped long ago", segs)
	}
	if rec.maxWrite != 0 {
		t.Fatalf("a compaction ran (largest write %d bytes); a sequential file is never copied", rec.maxWrite)
	}
}

// A crash at any step of a rotation leaves a log that replays to the acknowledged bytes:
// the new segment's create, its directory fsync and its first records failing, and a
// restart with an empty new head.
// Invariant: I-397, I-37, I-63
func TestWAL_Rotate_CrashPoints(t *testing.T) {
	cases := []struct {
		name string
		step sysfstest.Step
		nth  int
	}{
		{"create", sysfstest.StepOpenFile, 1},
		{"dir fsync", sysfstest.StepSyncDir, 1},
		{"first record write", sysfstest.StepWrite, 1},
		{"first fsync", sysfstest.StepSync, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ff := sysfstest.New(sysfs.OS{})
			ov, _ := newOverlay(t, dir, WithFS(ff), WithSegmentSize(4096))
			ino := meta.Ino(63)
			var want []byte
			for i := range 3 { // segment 0 just under its size: the next write rotates
				p := pattern(1500, byte(i))
				must(t, ov.Write(ino, 0, uint64(len(want)), p))
				want = append(want, p...)
			}
			ff.FailAtNth(c.step, c.nth, errors.New("injected"))
			p := pattern(700, 77)
			err := ov.Write(ino, 0, uint64(len(want)), p)
			if err == nil {
				want = append(want, p...) // the step did not run for this write: acknowledged
			}
			must(t, ov.Close())
			ov2, _ := newOverlay(t, dir)
			if got := readFrom(t, ov2, ino, 0, baseOf(nil), uint64(len(want))+100); !bytes.Equal(got, want) {
				t.Fatalf("after the crash (write err %v): differs at %d (len %d want %d)", err, firstDiff(got, want), len(got), len(want))
			}
			// and the log still takes writes
			must(t, ov2.Write(ino, 0, uint64(len(want)), []byte("more")))
		})
	}
}

// Compaction copies only a small live set: a region rewritten over and over compacts
// once the dead bytes pass the live bytes plus CompactMin, while a log with more than
// CompactMax live bytes is never copied however much of it is dead.
// Invariant: I-397, I-80
func TestWAL_Compact_OnlySmallLive(t *testing.T) {
	cs := uint64(testCS)
	t.Run("small live set compacts", func(t *testing.T) {
		rec := &recordingFS{FS: sysfs.OS{}}
		dir := t.TempDir()
		ov, _ := newOverlay(t, dir, WithFS(rec), WithSegmentSize(1<<20))
		ov.compactMin, ov.compactMax = 64<<10, 64<<10
		ino := meta.Ino(64)
		for i := range 200 { // the same 1 KiB rewritten: dead bytes pile up, live stays 1 KiB
			must(t, ov.Write(ino, 0, 10, pattern(1024, byte(i))))
		}
		plan, _ := ov.SnapshotForFlush(ino)
		must(t, ov.Write(ino, 0, 2*cs, pattern(512, 5))) // fresh: stays live
		must(t, ov.CommitFlushed(plan, 1034))
		if rec.maxWrite == 0 {
			t.Fatal("no compaction: 200 KiB dead against 512 B live")
		}
		if lb := logBytes(t, dir, ino); lb > 4096 {
			t.Fatalf("compacted log %d bytes", lb)
		}
	})
	t.Run("large live set is not copied", func(t *testing.T) {
		rec := &recordingFS{FS: sysfs.OS{}}
		dir := t.TempDir()
		ov, _ := newOverlay(t, dir, WithFS(rec), WithSegmentSize(1<<20))
		ov.compactMin, ov.compactMax = 16<<10, 64<<10
		ino := meta.Ino(65)
		for i := range 200 {
			must(t, ov.Write(ino, 0, 10, pattern(1024, byte(i))))
		}
		plan, _ := ov.SnapshotForFlush(ino)
		must(t, ov.Write(ino, 0, 4*cs, pattern(100<<10, 5))) // 100 KiB fresh: over CompactMax
		must(t, ov.CommitFlushed(plan, 1034))
		if rec.maxWrite != 0 {
			t.Fatalf("a compaction copied a live set over CompactMax (largest write %d)", rec.maxWrite)
		}
	})
}

// A file many times the old 128 MiB cap stages whole at local speed: with a stage larger
// than the file, every WRITE is accepted without waiting for a flush, and the flushes
// that follow never copy the log.
// Invariant: I-397, I-396
func TestOverlay_LargeStage_NoCopy(t *testing.T) {
	rec := &recordingFS{FS: sysfs.OS{}}
	dir := t.TempDir()
	const size = 6 << 20
	ov, _ := newOverlay(t, dir, WithFS(rec), WithSegmentSize(256<<10), WithMaxDirty(8<<20), WithWALCap(8<<20), WithCapWait(0))
	ino := meta.Ino(66)
	cs := uint64(testCS)
	for off := uint64(0); off < size; off += 32 << 10 {
		if err := ov.Write(ino, 0, off, pattern(32<<10, byte(off>>15))); err != nil {
			t.Fatalf("write at %d: %v (the stage holds the whole file)", off, err)
		}
	}
	var flushed uint64
	for flushed < size {
		plan, ok := ov.SnapshotForFlush(ino)
		if !ok {
			t.Fatal("no plan")
		}
		var idxs []uint64
		for i := flushed / cs; i < flushed/cs+64 && i < size/cs; i++ {
			idxs = append(idxs, i)
		}
		commitChunks(plan, idxs, cs)
		flushed += uint64(len(idxs)) * cs
		must(t, ov.CommitFlushed(plan, flushed))
	}
	if rec.maxWrite != 0 {
		t.Fatalf("the log was copied (largest write %d)", rec.maxWrite)
	}
	if n := len(segFiles(t, dir, ino)); n != 0 {
		t.Fatalf("%d segments left after the whole file committed", n)
	}
}

// Randomised model of the log: writes, truncates, partial and full commits, aborts and
// restarts at random points, with small segments so rotations and drops happen often.
// After every step the overlay reads exactly the model, and a restart replays it.
// Invariant: I-397, I-37, I-34
func TestOverlay_ReplayModel(t *testing.T) {
	rng := testseed.Seed(t)
	ops := 800
	if !testing.Short() {
		ops = 20000
	}
	dir := t.TempDir()
	open := func() *Overlay {
		ov, _ := newOverlay(t, dir, WithSegmentSize(8<<10))
		ov.compactMin, ov.compactMax = 32<<10, 16<<10
		return ov
	}
	ov := open()
	cs := uint64(testCS)
	ino := meta.Ino(67)
	var base, model, planModel []byte
	var plan *FlushPlan
	check := func(i int, when string) {
		t.Helper()
		got := readFrom(t, ov, ino, uint64(len(base)), baseOf(base), uint64(len(model))+10)
		if !bytes.Equal(got, model) {
			t.Fatalf("op %d %s: differs at %d (len %d want %d)", i, when, firstDiff(got, model), len(got), len(model))
		}
	}
	must(t, ov.Truncate(ino, 0, 0))
	for i := range ops {
		switch r := rng.Intn(20); {
		case r < 10: // a write, mostly sequential
			off := uint64(len(model))
			if rng.Intn(4) == 0 && len(model) > 0 {
				off = uint64(rng.Intn(len(model)))
			}
			data := pattern(1+rng.Intn(3000), byte(rng.Intn(256)))
			must(t, ov.Write(ino, uint64(len(base)), off, data))
			if end := off + uint64(len(data)); end > uint64(len(model)) {
				model = append(model, make([]byte, end-uint64(len(model)))...)
			}
			copy(model[off:], data)
		case r < 11: // a truncate
			size := uint64(rng.Intn(len(model) + 1))
			must(t, ov.Truncate(ino, uint64(len(base)), size))
			model = model[:size]
		case r < 14: // capture a plan
			if plan == nil {
				if p, ok := ov.SnapshotForFlush(ino); ok {
					plan, planModel = p, bytes.Clone(model)
				}
			}
		case r < 17: // commit it, partially when it has no truncate
			if plan == nil {
				break
			}
			if plan.SizeSet || rng.Intn(3) == 0 {
				must(t, ov.CommitFlushed(plan, uint64(len(planModel))))
				base = planModel
			} else {
				// commit a prefix of the plan's whole chunks
				full := uint64(len(planModel)) / cs
				k := uint64(rng.Intn(int(full) + 1))
				idxs := make([]uint64, 0, k)
				for j := range k {
					if plan.Extents.covers(j*cs, (j+1)*cs) || j*cs < uint64(len(base)) {
						idxs = append(idxs, j)
					}
				}
				commitChunks(plan, idxs, cs)
				// the committed chunks hold the plan's bytes; the rest of the base is as before
				newBase := bytes.Clone(base)
				committedEnd := uint64(len(base))
				for _, j := range idxs {
					committedEnd = max(committedEnd, min((j+1)*cs, uint64(len(planModel))))
				}
				if committedEnd > uint64(len(newBase)) {
					newBase = append(newBase, make([]byte, committedEnd-uint64(len(newBase)))...)
				}
				for _, j := range idxs {
					lo, hi := j*cs, min((j+1)*cs, uint64(len(planModel)))
					copy(newBase[lo:hi], planModel[lo:hi])
				}
				must(t, ov.CommitFlushed(plan, uint64(len(newBase))))
				base = newBase
			}
			plan, planModel = nil, nil
		case r < 18: // the flush fails
			if plan != nil {
				ov.AbortFlush(plan)
				plan, planModel = nil, nil
			}
		default: // a restart: an outstanding plan is lost, its bytes still in the log
			if plan != nil {
				ov.AbortFlush(plan)
				plan, planModel = nil, nil
			}
			must(t, ov.Close())
			ov = open()
			check(i, "after restart")
		}
		check(i, "")
	}
	if plan != nil {
		ov.AbortFlush(plan)
	}
	must(t, ov.Close())
	ov = open()
	check(ops, "final restart")
}

// walName is segment 0's file name for ino.
func walName(ino meta.Ino) string { return strconv.FormatUint(uint64(ino), 10) + ".wal" }
