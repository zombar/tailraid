package vfs

import (
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

var allAttrs = meta.AttrMode | meta.AttrUid | meta.AttrGid | meta.AttrAtime | meta.AttrMtime

func sa(mask uint8, mode uint32, atime, mtime int64) meta.SetAttr {
	return meta.SetAttr{Mask: mask, Mode: mode, Uid: 501, Gid: 20, Atime: atime, Mtime: mtime}
}

func holds(t *testing.T, ov *Overlay, ino meta.Ino, s meta.SetAttr, want bool) {
	t.Helper()
	held, err := ov.HoldAttrs(ino, 0, s)
	if err != nil || held != want {
		t.Fatalf("HoldAttrs %+v: held %v err %v, want held %v", s, held, err, want)
	}
}

func attrsOf(t *testing.T, ov *Overlay, ino meta.Ino) meta.CommitAttrs {
	t.Helper()
	a, _ := ov.Attrs(ino)
	return a
}

// A SETATTR is held only on a file with unflushed data here, only for its writer, only
// for mode, owner and times; a held one is what the gateway shows at once and a restart
// replays it.
// Invariant: I-401
func TestOverlay_HoldAttrs_DirtyOnly(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(81)
	holds(t, ov, ino, sa(meta.AttrMode, 0o600, 0, 0), false) // nothing dirty: proposed instead
	must(t, ov.Write(ino, 0, 10, []byte("data")))
	holds(t, ov, ino, sa(meta.AttrFlags, 0, 0, 0), false) // flags are a directory's
	if held, _ := ov.HoldAttrs(ino, 7, sa(meta.AttrMode, 0o600, 0, 0)); held {
		t.Fatal("another writer's SETATTR was held: the Commit acts for the writer (I-401)")
	}
	holds(t, ov, ino, sa(meta.AttrMode|meta.AttrUid|meta.AttrGid, 0o640, 0, 0), true)
	holds(t, ov, ino, sa(meta.AttrAtime|meta.AttrMtime, 0, 111, 222), true)
	want := meta.CommitAttrs{Mask: allAttrs, Mode: 0o640, Uid: 501, Gid: 20, Atime: 111, Mtime: 222}
	if got := attrsOf(t, ov, ino); got != want {
		t.Fatalf("pending %+v, want %+v", got, want)
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := attrsOf(t, ov2, ino); got != want {
		t.Fatalf("after replay %+v, want %+v", got, want)
	}
}

// A write or truncate after held times drops them (the flush's time wins, I-230) and keeps
// the mode and owner, also when the times sit in an outstanding plan; a restart agrees.
// Invariant: I-401, I-230
func TestOverlay_HoldAttrs_WriteDropsTimes(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(82)
	must(t, ov.Write(ino, 0, 10, []byte("data")))
	holds(t, ov, ino, sa(meta.AttrMode|meta.AttrAtime|meta.AttrMtime, 0o600, 5, 6), true)
	must(t, ov.Write(ino, 0, 20, []byte("more")))
	if got := attrsOf(t, ov, ino); got.Mask != meta.AttrMode || got.Mode != 0o600 {
		t.Fatalf("after a write: %+v, want the mode alone", got)
	}
	holds(t, ov, ino, sa(meta.AttrMtime, 0, 0, 9), true)
	plan, _ := ov.SnapshotForFlush(ino)
	must(t, ov.Truncate(ino, 0, 15)) // newer than the times in the plan
	if got := ov.PlanAttrs(plan); got.Mask != meta.AttrMode {
		t.Fatalf("plan attributes after a truncate: %+v, want the mode alone", got)
	}
	ov.AbortFlush(plan)
	if got := attrsOf(t, ov, ino); got.Mask != meta.AttrMode || got.Mode != 0o600 {
		t.Fatalf("after the abort: %+v", got)
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := attrsOf(t, ov2, ino); got.Mask != meta.AttrMode || got.Mode != 0o600 {
		t.Fatalf("after replay: %+v", got)
	}
}

// A plan takes the held attributes; a full commit lands them and leaves none pending, a
// partial commit or an abort leaves them pending (fresh ones win), and every step replays
// to the same pending set.
// Invariant: I-401
func TestOverlay_Attrs_CaptureCommitAbort(t *testing.T) {
	cs := uint64(testCS)
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(83)
	reopen := func(want meta.CommitAttrs, when string) {
		t.Helper()
		if got := attrsOf(t, ov, ino); got != want {
			t.Fatalf("%s: pending %+v, want %+v", when, got, want)
		}
		must(t, ov.Close())
		ov, _ = newOverlay(t, dir)
		if got := attrsOf(t, ov, ino); got != want {
			t.Fatalf("%s, after replay: pending %+v, want %+v", when, got, want)
		}
	}
	must(t, ov.Write(ino, 0, 0, pattern(int(2*cs+10), 1)))
	holds(t, ov, ino, sa(meta.AttrMode, 0o600, 0, 0), true)
	// a partial commit of chunk 0 keeps the attributes pending
	plan, _ := ov.SnapshotForFlush(ino)
	if got := ov.PlanAttrs(plan); got.Mask != meta.AttrMode {
		t.Fatalf("plan attributes %+v", got)
	}
	holds(t, ov, ino, sa(meta.AttrMtime, 0, 0, 77), true) // fresh, during the flush
	commitChunks(plan, []uint64{0}, cs)
	must(t, ov.CommitFlushed(plan, cs))
	reopen(meta.CommitAttrs{Mask: meta.AttrMode | meta.AttrMtime, Mode: 0o600, Mtime: 77}, "after a partial commit")
	// a full commit lands them: nothing pending, the inode clean
	plan, _ = ov.SnapshotForFlush(ino)
	must(t, ov.CommitFlushed(plan, 2*cs+10))
	if _, ok := ov.Attrs(ino); ok {
		t.Fatal("attributes still pending after the full commit")
	}
	if len(ov.Dirty()) != 0 {
		t.Fatalf("still dirty: %+v", ov.Dirty())
	}
}

// Held attributes outlive the segment their record is in, and do not pin it: a file
// written front to back keeps shedding segments while an attribute is held, which each
// rotation carries into the new segment, and a restart still has it.
// Invariant: I-401, I-397
func TestOverlay_Attrs_PinTheirSegment(t *testing.T) {
	cs := uint64(testCS)
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir, WithSegmentSize(8<<10))
	ino := meta.Ino(84)
	must(t, ov.Write(ino, 0, 0, pattern(int(cs), 1)))
	holds(t, ov, ino, sa(meta.AttrMode, 0o640, 0, 0), true)
	var flushed uint64
	for round := range 12 {
		off := flushed + cs
		for range 4 {
			must(t, ov.Write(ino, flushed, off, pattern(int(cs), byte(round))))
			off += cs
		}
		plan, _ := ov.SnapshotForFlush(ino)
		idx := flushed / cs
		commitChunks(plan, []uint64{idx, idx + 1, idx + 2}, cs) // the newest chunks stay dirty
		flushed += 3 * cs
		must(t, ov.CommitFlushed(plan, flushed))
		// and the attributes move with the log: the segments behind them still go
		var dirty uint64
		for _, d := range ov.Dirty() {
			dirty += d.DirtyBytes
		}
		if lb := logBytes(t, dir, ino); round > 2 && lb > int64(dirty)+3*(8<<10)+4096 {
			t.Fatalf("round %d: log %d bytes for %d dirty: a held attribute pins old segments", round, lb, dirty)
		}
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := attrsOf(t, ov2, ino); got.Mask != meta.AttrMode || got.Mode != 0o640 {
		t.Fatalf("after many segments and a restart: %+v", got)
	}
}
