package vfs

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

const testCS = uint32(4096)

func newOverlay(t *testing.T, dir string, opts ...Option) (*Overlay, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	ov, err := OpenOverlay(dir, testCS, clk, opts...)
	if err != nil {
		t.Fatalf("OpenOverlay: %v", err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	return ov, clk
}

// baseOf returns a base reader over fixed bytes (what the committed chunks would hold).
func baseOf(b []byte) func(off uint64, p []byte) error {
	return func(off uint64, p []byte) error {
		if off >= uint64(len(b)) {
			for i := range p {
				p[i] = 0
			}
			return nil
		}
		n := copy(p, b[off:])
		for i := n; i < len(p); i++ {
			p[i] = 0
		}
		return nil
	}
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// readFrom reads up to n bytes from offset 0 with the given committed size (what the FSM
// holds); the overlay decides the visible size from it and its own state.
// A clean entry is dropped after its flush, so the read falls back to the committed bytes
// alone, as the filesystem layer does.
func readFrom(t *testing.T, ov *Overlay, ino meta.Ino, committed uint64, base func(uint64, []byte) error, n uint64) []byte {
	t.Helper()
	out := make([]byte, n)
	got, err := ov.Read(ino, committed, 0, out, base)
	if errors.Is(err, meta.ErrNotExist) {
		out = out[:min(n, committed)]
		if err := base(0, out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Read: %v", err)
	}
	return out[:got]
}

// readAll is readFrom with the committed size equal to the read length (fine while the
// entry has a truncate recorded, which pins the size).
func readAll(t *testing.T, ov *Overlay, ino meta.Ino, base func(uint64, []byte) error, size uint64) []byte {
	t.Helper()
	out := make([]byte, size)
	n, err := ov.Read(ino, size, 0, out, base)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Read: %v", err)
	}
	return out[:n]
}

// Invariant: I-34
// Lifecycle: L-2, L-72
func TestOverlay_WriteAtNonZeroOffset_PreservesBase(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	base := pattern(10000, 1)
	ino := meta.Ino(5)
	if err := ov.Write(ino, 10000, 3000, []byte("HELLO")); err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(base)
	copy(want[3000:], "HELLO")
	got := readAll(t, ov, ino, baseOf(base), 10000)
	if !bytes.Equal(got, want) {
		t.Fatalf("content differs at %d", firstDiff(got, want))
	}
	if sz, ok := ov.Size(ino, 10000); !ok || sz != 10000 {
		t.Fatalf("Size = %d,%v", sz, ok)
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// Lifecycle: L-6, L-72
func TestOverlay_ReadMergesExtentsOverBase(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	base := pattern(100, 9)
	ino := meta.Ino(1)
	must(t, ov.Write(ino, 100, 10, bytes.Repeat([]byte("a"), 20))) // [10,30)
	must(t, ov.Write(ino, 100, 25, bytes.Repeat([]byte("b"), 10))) // [25,35) overrides tail of a
	must(t, ov.Write(ino, 100, 90, bytes.Repeat([]byte("c"), 20))) // [90,110) extends to 110
	want := bytes.Clone(base)
	want = append(want, make([]byte, 10)...)
	copy(want[10:], bytes.Repeat([]byte("a"), 20))
	copy(want[25:], bytes.Repeat([]byte("b"), 10))
	copy(want[90:], bytes.Repeat([]byte("c"), 20))
	got := readAll(t, ov, ino, baseOf(base), 110)
	if !bytes.Equal(got, want) {
		t.Fatalf("content differs at %d: got %q want %q", firstDiff(got, want), got, want)
	}
	// partial read in the middle, spanning base/extent boundaries
	buf := make([]byte, 30)
	n, err := ov.Read(ino, uint64(len(base)), 20, buf, baseOf(base))
	if err != nil || n != 30 || !bytes.Equal(buf, want[20:50]) {
		t.Fatalf("mid read n=%d err=%v got %q want %q", n, err, buf, want[20:50])
	}
	// read at EOF
	n, err = ov.Read(ino, uint64(len(base)), 110, buf, baseOf(base))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF: n=%d err=%v", n, err)
	}
	// short read crossing EOF returns io.EOF with the bytes
	n, err = ov.Read(ino, uint64(len(base)), 100, buf, baseOf(base))
	if n != 10 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:10], want[100:]) {
		t.Fatalf("read crossing EOF: n=%d err=%v", n, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Review bug #2: truncate down then grow must never resurface old base bytes.
// Lifecycle: L-8, L-11, L-12
func TestOverlay_TruncateThenGrow_ZerosOldBytes(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	cs := uint64(testCS)
	baseSize := 10 * cs
	base := pattern(int(baseSize), 3)
	ino := meta.Ino(42)
	trunc := cs + cs/2
	must(t, ov.Truncate(ino, baseSize, trunc))
	must(t, ov.Write(ino, baseSize, 5*cs, []byte("tail")))

	want := make([]byte, 5*cs+4)
	copy(want, base[:trunc])
	copy(want[5*cs:], "tail")
	got := readAll(t, ov, ino, baseOf(base), 5*cs+4)
	if !bytes.Equal(got, want) {
		t.Fatalf("content differs at %d (trunc point %d)", firstDiff(got, want), trunc)
	}
	if ms, ok := ov.MinSize(ino); !ok || ms != trunc {
		t.Fatalf("MinSize = %d,%v want %d", ms, ok, trunc)
	}
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("no plan")
	}
	if !plan.SizeSet || plan.MinSize != trunc || plan.Size != 5*cs+4 {
		t.Fatalf("plan = %+v", plan)
	}
	// the flusher relies on this: chunks 1 (straddling) must be rewritten, 2..4 are HashZero
	if plan.Extents.coversChunk(5, testCS, plan.Size) != true {
		t.Fatalf("chunk 5 should be covered by the tail write")
	}
}

// Lifecycle: L-8, L-11
func TestOverlay_TruncateDownThenUp(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	base := pattern(1000, 5)
	ino := meta.Ino(2)
	must(t, ov.Truncate(ino, 1000, 300))
	must(t, ov.Truncate(ino, 1000, 800))
	want := make([]byte, 800)
	copy(want, base[:300])
	got := readAll(t, ov, ino, baseOf(base), 800)
	if !bytes.Equal(got, want) {
		t.Fatalf("truncate down/up differs at %d", firstDiff(got, want))
	}
	if ms, _ := ov.MinSize(ino); ms != 300 {
		t.Fatalf("MinSize = %d", ms)
	}
	if sz, _ := ov.Size(ino, 1000); sz != 800 {
		t.Fatalf("Size = %d", sz)
	}
}

// Lifecycle: L-3, L-4
func TestOverlay_WritePastEOF_ReadsZerosInGap(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	base := pattern(100, 7)
	ino := meta.Ino(3)
	must(t, ov.Write(ino, 100, 500, []byte("far")))
	want := make([]byte, 503)
	copy(want, base)
	copy(want[500:], "far")
	got := readAll(t, ov, ino, baseOf(base), 503)
	if !bytes.Equal(got, want) {
		t.Fatalf("gap not zero at %d", firstDiff(got, want))
	}
	// the gap below base size but above an earlier truncate is also zero
	must(t, ov.Truncate(ino, 100, 50))
	must(t, ov.Write(ino, 100, 90, []byte("x")))
	want = make([]byte, 91)
	copy(want, base[:50])
	want[90] = 'x'
	got = readAll(t, ov, ino, baseOf(base), 91)
	if !bytes.Equal(got, want) {
		t.Fatalf("post-truncate gap differs at %d", firstDiff(got, want))
	}
}

// Lifecycle: L-1, L-5
func TestOverlay_WriteSpanningChunkBoundary(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(11)
	cs := uint64(testCS)
	data := pattern(100, 1)
	must(t, ov.Write(ino, 0, cs-50, data))
	got := readAll(t, ov, ino, baseOf(nil), cs+50)
	if !bytes.Equal(got[cs-50:], data) || !bytes.Equal(got[:cs-50], make([]byte, cs-50)) {
		t.Fatal("spanning write content wrong")
	}
	plan, _ := ov.SnapshotForFlush(ino)
	// chunk 0 keeps 4046 base bytes (not covered); chunk 1 is [cs, cs+50) and entirely dirty
	if plan.Extents.coversChunk(0, testCS, plan.Size) || !plan.Extents.coversChunk(1, testCS, plan.Size) {
		t.Fatal("chunk 0 must not be covered, chunk 1 must be")
	}
	if len(plan.Extents.overlapping(0, cs)) != 1 || len(plan.Extents.overlapping(cs, cs)) != 1 {
		t.Fatal("extent should overlap both chunks")
	}
}

func TestOverlay_SnapshotForFlush_NewWritesGoToFreshSet(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(4)
	must(t, ov.Write(ino, 0, 0, []byte("aaaa")))
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok || len(plan.Extents) != 1 || plan.Size != 4 {
		t.Fatalf("plan = %+v ok=%v", plan, ok)
	}
	must(t, ov.Write(ino, 0, 2, []byte("BB")))
	// plan unaffected; live view sees both
	if len(plan.Extents) != 1 || plan.Extents[0].n != 4 {
		t.Fatalf("plan mutated: %+v", plan.Extents)
	}
	got := readAll(t, ov, ino, baseOf(nil), 4)
	if string(got) != "aaBB" {
		t.Fatalf("live read %q", got)
	}
	d := ov.Dirty()
	if len(d) != 1 || !d[0].Flushing || d[0].DirtyBytes != 2 {
		t.Fatalf("Dirty = %+v", d)
	}
}

// Invariant: I-35
func TestOverlay_AbortFlush_MergesBackNewerWins(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(6)
	must(t, ov.Write(ino, 0, 0, []byte("aaaaaaaa"))) // [0,8)
	plan, _ := ov.SnapshotForFlush(ino)
	must(t, ov.Write(ino, 0, 2, []byte("BB"))) // fresh [2,4)
	must(t, ov.Truncate(ino, 0, 6))            // fresh truncate: plan bytes beyond 6 must not come back
	ov.AbortFlush(plan)
	got := readAll(t, ov, ino, baseOf(nil), 6)
	if string(got) != "aaBBaa" {
		t.Fatalf("after abort %q", got)
	}
	if sz, _ := ov.Size(ino, 0); sz != 6 {
		t.Fatalf("Size %d", sz)
	}
	if ms, _ := ov.MinSize(ino); ms != 6 {
		t.Fatalf("MinSize %d (the fresh truncate point must be kept)", ms)
	}
	d := ov.Dirty()
	if len(d) != 1 || d[0].Flushing || d[0].DirtyBytes != 6 {
		t.Fatalf("Dirty = %+v", d)
	}
	// a second snapshot now contains everything, and the WAL still holds all live bytes
	plan2, ok := ov.SnapshotForFlush(ino)
	if !ok || plan2.Extents.bytes() != 6 || !plan2.SizeSet || plan2.MinSize != 6 {
		t.Fatalf("plan2 = %+v", plan2)
	}
}

// Lifecycle: L-62
func TestOverlay_CommitFlushed_ResetsOrCompacts(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(8)
	big := bytes.Repeat([]byte("x"), 50000)
	must(t, ov.Write(ino, 0, 0, big))
	plan, _ := ov.SnapshotForFlush(ino)
	must(t, ov.CommitFlushed(plan, 50000))
	// nothing left to flush: the entry and its WAL are gone (L-178), reads go to the base
	if _, err := os.Stat(filepath.Join(dir, "8.wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("WAL not removed after a clean commit: %v", err)
	}
	if len(ov.Dirty()) != 0 || ov.TotalDirty() != 0 {
		t.Fatalf("still dirty: %+v total=%d", ov.Dirty(), ov.TotalDirty())
	}
	if _, ok := ov.Size(ino, 50000); ok {
		t.Fatal("clean inode still tracked")
	}

	// now a flush with concurrent fresh writes: WAL is compacted to the fresh bytes only
	must(t, ov.Write(ino, 50000, 100, bytes.Repeat([]byte("y"), 40000)))
	plan, _ = ov.SnapshotForFlush(ino)
	must(t, ov.Write(ino, 50000, 10, []byte("fresh")))
	must(t, ov.CommitFlushed(plan, 50000))
	st, err := os.Stat(filepath.Join(dir, "8.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 1000 {
		t.Fatalf("WAL not compacted: %d bytes", st.Size())
	}
	want := bytes.Clone(big)
	copy(want[100:], bytes.Repeat([]byte("y"), 40000)) // committed → delivered by base
	copy(want[10:], "fresh")
	got := readAll(t, ov, ino, baseOf(want), 50000)
	// base callback already includes the y's (committed); the overlay must add "fresh" only
	if !bytes.Equal(got, want) {
		t.Fatalf("after compaction differs at %d", firstDiff(got, want))
	}
	if ov.TotalDirty() != 5 {
		t.Fatalf("TotalDirty = %d", ov.TotalDirty())
	}
}

func TestOverlay_CommitFlushed_WithResidual(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(12)
	cs := uint64(testCS)
	full := pattern(int(cs), 2)
	must(t, ov.Write(ino, 0, 0, full))                  // chunk 0 fully covered
	must(t, ov.Write(ino, 0, cs+10, []byte("partial"))) // chunk 1 partial
	plan, _ := ov.SnapshotForFlush(ino)
	// flusher flushed only chunk 0 (partial flush); chunk 1's extent is residual
	plan.Residual = plan.Extents.overlapping(cs, cs)
	must(t, ov.CommitFlushed(plan, cs))
	d := ov.Dirty()
	if len(d) != 1 || d[0].DirtyBytes != 7 || d[0].Size != cs+17 {
		t.Fatalf("Dirty = %+v", d)
	}
	base := make([]byte, cs)
	copy(base, full)
	want := make([]byte, cs+17)
	copy(want, base)
	copy(want[cs+10:], "partial")
	got := readAll(t, ov, ino, baseOf(base), cs+17)
	if !bytes.Equal(got, want) {
		t.Fatalf("residual content differs at %d", firstDiff(got, want))
	}
}

func TestOverlay_OnlyOnePlanOutstanding(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(9)
	must(t, ov.Write(ino, 0, 0, []byte("a")))
	p1, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("first plan")
	}
	must(t, ov.Write(ino, 0, 1, []byte("b")))
	if p2, ok := ov.SnapshotForFlush(ino); ok || p2 != nil {
		t.Fatal("second plan while first outstanding")
	}
	must(t, ov.CommitFlushed(p1, 1))
	if _, ok := ov.SnapshotForFlush(ino); !ok {
		t.Fatal("plan after commit")
	}
	// clean inode yields no plan
	ino2 := meta.Ino(10)
	if _, ok := ov.SnapshotForFlush(ino2); ok {
		t.Fatal("plan for unknown inode")
	}
}

// Invariant: I-7, I-37
// Lifecycle: L-113, L-114
func TestOverlay_RestartReplaysDirtyWALs(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	base := pattern(5000, 4)
	a, b := meta.Ino(21), meta.Ino(22)
	must(t, ov.Write(a, 5000, 100, []byte("alpha")))
	must(t, ov.Truncate(a, 5000, 3000))
	must(t, ov.Write(a, 5000, 4000, []byte("omega")))
	must(t, ov.Write(b, 0, 0, bytes.Repeat([]byte("b"), 10)))
	plan, _ := ov.SnapshotForFlush(b)
	must(t, ov.CommitFlushed(plan, 10))
	must(t, ov.Write(b, 10, 20, []byte("c"))) // fresh dirty after a commit
	wantA := readAll(t, ov, a, baseOf(base), 4005)
	wantB := readAll(t, ov, b, baseOf(bytes.Repeat([]byte("b"), 10)), 21)
	must(t, ov.Close())

	// torn tail: append garbage to a's WAL (simulated crash mid-record, L-113)
	f, _ := os.OpenFile(filepath.Join(dir, "21.wal"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19})
	_ = f.Close()

	ov2, _ := newOverlay(t, dir)
	gotA := readAll(t, ov2, a, baseOf(base), 4005)
	gotB := readAll(t, ov2, b, baseOf(bytes.Repeat([]byte("b"), 10)), 21)
	if !bytes.Equal(gotA, wantA) {
		t.Fatalf("a differs at %d after restart", firstDiff(gotA, wantA))
	}
	if !bytes.Equal(gotB, wantB) {
		t.Fatalf("b differs at %d after restart", firstDiff(gotB, wantB))
	}
	if ms, _ := ov2.MinSize(a); ms != 3000 {
		t.Fatalf("a MinSize after restart %d", ms)
	}
	if sz, _ := ov2.Size(a, 0); sz != 4005 {
		t.Fatalf("a Size after restart %d", sz)
	}
	d := ov2.Dirty()
	if len(d) != 2 {
		t.Fatalf("Dirty after restart = %+v", d)
	}
	if ov2.TotalDirty() != 5+5+1 {
		t.Fatalf("TotalDirty after restart = %d", ov2.TotalDirty())
	}
	// a plan after restart still carries the truncate
	plan, ok := ov2.SnapshotForFlush(a)
	if !ok || !plan.SizeSet || plan.MinSize != 3000 {
		t.Fatalf("plan after restart = %+v", plan)
	}
}

// Lifecycle: L-13
func TestOverlay_DropRemovesWAL(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(30)
	must(t, ov.Write(ino, 0, 0, []byte("gone")))
	ov.Drop(ino)
	if _, err := os.Stat(filepath.Join(dir, "30.wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("WAL still exists: %v", err)
	}
	if _, ok := ov.Size(ino, 0); ok {
		t.Fatal("inode still tracked")
	}
	if ov.TotalDirty() != 0 || len(ov.Dirty()) != 0 {
		t.Fatal("still dirty after drop")
	}
	ov.Drop(ino) // idempotent
}

// Lifecycle: L-23
func TestOverlay_WALCapForcesFlush(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir(), WithWALCap(1000))
	ino := meta.Ino(31)
	must(t, ov.Write(ino, 0, 0, bytes.Repeat([]byte("w"), 900)))
	err := ov.Write(ino, 0, 0, bytes.Repeat([]byte("w"), 900))
	if !errors.Is(err, ErrWALFull) {
		t.Fatalf("second write err = %v", err)
	}
	// after a flush the cap is released
	plan, _ := ov.SnapshotForFlush(ino)
	must(t, ov.CommitFlushed(plan, 900))
	must(t, ov.Write(ino, 900, 0, bytes.Repeat([]byte("w"), 900)))
}

func TestOverlay_ConcurrentWritersDifferentInodes(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(ino meta.Ino) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := ov.Write(ino, 0, uint64(j*10), []byte("0123456789")); err != nil {
					t.Error(err)
					return
				}
				buf := make([]byte, 10)
				if _, err := ov.Read(ino, 0, uint64(j*10), buf, baseOf(nil)); err != nil && !errors.Is(err, io.EOF) {
					t.Error(err)
				}
			}
			_ = ov.Dirty()
			_ = ov.TotalDirty()
		}(meta.Ino(i))
	}
	wg.Wait()
	if ov.TotalDirty() != 8*500 {
		t.Fatalf("TotalDirty = %d", ov.TotalDirty())
	}
}

func TestOverlay_DirtyListing(t *testing.T) {
	ov, clk := newOverlay(t, t.TempDir())
	must(t, ov.Write(meta.Ino(1), 0, 0, []byte("ab")))
	clk.Advance(3 * time.Second)
	must(t, ov.Write(meta.Ino(2), 0, 0, []byte("cde")))
	d := ov.Dirty()
	if len(d) != 2 {
		t.Fatalf("Dirty = %+v", d)
	}
	byIno := map[meta.Ino]DirtyInfo{}
	for _, x := range d {
		byIno[x.Ino] = x
	}
	if byIno[1].DirtyBytes != 2 || byIno[2].DirtyBytes != 3 {
		t.Fatalf("bytes: %+v", byIno)
	}
	if !byIno[2].LastWrite.After(byIno[1].LastWrite) {
		t.Fatalf("lastWrite ordering: %+v", byIno)
	}
	// a truncate alone makes an inode dirty
	must(t, ov.Truncate(meta.Ino(3), 100, 50))
	if len(ov.Dirty()) != 3 {
		t.Fatal("truncate-only inode not listed")
	}
}

// Model-vs-overlay randomized oracle, including the flush lifecycle (snapshot, commit
// with the plan's bytes becoming the base, abort) and periodic restarts.
// Invariant: I-34, I-37
// Lifecycle: L-1, L-2, L-3, L-4, L-5, L-6, L-8, L-11, L-12, L-172, L-180, L-181
func TestOverlay_OracleRandomOps(t *testing.T) {
	rng := seed(t)
	ops := 600
	if !testing.Short() {
		ops = 20000
	}
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	cs := uint64(testCS)
	type file struct {
		base      []byte // committed bytes; replaced by a committed plan's bytes
		model     []byte
		plan      *FlushPlan
		planModel []byte // the model at snapshot time: what a full flush commits
	}
	files := map[meta.Ino]*file{}
	for i := 1; i <= 4; i++ {
		n := rng.Intn(int(3 * cs))
		files[meta.Ino(i)] = &file{base: pattern(n, byte(i)), model: pattern(n, byte(i))}
		// The overlay only knows a file's size once it has been touched; a same-size
		// truncate is how a gateway opens a file for writing without changing it.
		must(t, ov.Truncate(meta.Ino(i), uint64(n), uint64(n)))
	}
	visibleSize := func(ino meta.Ino, f *file) uint64 {
		if sz, ok := ov.Size(ino, uint64(len(f.base))); ok {
			return sz
		}
		return uint64(len(f.base))
	}
	checkAll := func(i int, when string) {
		t.Helper()
		for ino, f := range files {
			got := readFrom(t, ov, ino, uint64(len(f.base)), baseOf(f.base), uint64(len(f.model))+10)
			if !bytes.Equal(got, f.model) {
				t.Fatalf("op %d %s ino %d: full content differs at %d (len got %d want %d)", i, when, ino, firstDiff(got, f.model), len(got), len(f.model))
			}
			if sz := visibleSize(ino, f); sz != uint64(len(f.model)) {
				t.Fatalf("op %d %s ino %d: Size %d want %d", i, when, ino, sz, len(f.model))
			}
		}
	}
	pick := func(f *file) uint64 {
		size := uint64(len(f.model))
		switch rng.Intn(7) {
		case 0:
			return 0
		case 1:
			return size
		case 2:
			return size + uint64(rng.Intn(int(cs)))
		case 3:
			return (uint64(rng.Intn(4)) * cs) + cs - 1
		case 4:
			return uint64(rng.Intn(4)) * cs
		case 5:
			return uint64(rng.Intn(4))*cs + 1
		default:
			if size == 0 {
				return 0
			}
			return uint64(rng.Intn(int(size)))
		}
	}
	for i := 0; i < ops; i++ {
		ino := meta.Ino(1 + rng.Intn(4))
		f := files[ino]
		switch r := rng.Intn(13); {
		case r == 10: // snapshot for flush
			if f.plan == nil {
				if p, ok := ov.SnapshotForFlush(ino); ok {
					f.plan, f.planModel = p, bytes.Clone(f.model)
				}
			}
		case r == 11: // the flush commits: the plan's bytes become the base
			if f.plan != nil {
				must(t, ov.CommitFlushed(f.plan, uint64(len(f.planModel))))
				f.base, f.plan, f.planModel = f.planModel, nil, nil
			}
		case r == 12: // the flush fails: everything stays dirty
			if f.plan != nil {
				ov.AbortFlush(f.plan)
				f.plan, f.planModel = nil, nil
			}
		case r < 6: // write
			off := pick(f)
			n := 1 + rng.Intn(int(cs/2))
			data := pattern(n, byte(rng.Intn(256)))
			must(t, ov.Write(ino, uint64(len(f.base)), off, data))
			if end := off + uint64(n); end > uint64(len(f.model)) {
				f.model = append(f.model, make([]byte, end-uint64(len(f.model)))...)
			}
			copy(f.model[off:], data)
		case r < 8: // truncate
			size := pick(f)
			must(t, ov.Truncate(ino, uint64(len(f.base)), size))
			if size < uint64(len(f.model)) {
				f.model = f.model[:size]
			} else {
				f.model = append(f.model, make([]byte, size-uint64(len(f.model)))...)
			}
		default: // read a random range
			off := pick(f)
			n := 1 + rng.Intn(int(cs))
			buf := make([]byte, n)
			got, err := ov.Read(ino, uint64(len(f.base)), off, buf, baseOf(f.base))
			if errors.Is(err, meta.ErrNotExist) { // clean entry dropped: committed bytes only
				got, err = 0, nil
				if off < uint64(len(f.base)) {
					got = min(n, len(f.base)-int(off))
					must(t, baseOf(f.base)(off, buf[:got]))
				}
			}
			wantN := 0
			if off < uint64(len(f.model)) {
				wantN = min(n, len(f.model)-int(off))
			}
			if got != wantN || (err != nil && !errors.Is(err, io.EOF)) {
				t.Fatalf("op %d: Read(%d,%d) n=%d err=%v want n=%d", i, off, n, got, err, wantN)
			}
			if got > 0 && !bytes.Equal(buf[:got], f.model[off:off+uint64(got)]) {
				t.Fatalf("op %d: Read(%d,%d) differs at %d", i, off, n, firstDiff(buf[:got], f.model[off:off+uint64(got)]))
			}
		}
		if i%250 == 249 {
			checkAll(i, "periodic")
		}
		if i%300 == 299 {
			// restart equivalence: a plan does not survive (its bytes are still in the WAL)
			for _, f := range files {
				if f.plan != nil {
					ov.AbortFlush(f.plan)
					f.plan, f.planModel = nil, nil
				}
			}
			must(t, ov.Close())
			ov, _ = newOverlay(t, dir)
			checkAll(i, "after restart")
		}
	}
	for _, f := range files {
		if f.plan != nil {
			ov.AbortFlush(f.plan)
			f.plan, f.planModel = nil, nil
		}
	}
	must(t, ov.Close())
	ov, _ = newOverlay(t, dir)
	checkAll(ops, "final restart")
	_ = rand.Int
}

// A truncate below the plan's extents while the flush is in progress: reads must not show
// the plan's bytes at or beyond the truncation point, before or after the commit, and a
// later write past that point reads back with zeros in the gap.
// Invariant: I-34
// Lifecycle: L-172, L-181
func TestOverlay_TruncateDuringFlush_HidesPlanBytes(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(32)
	planned := pattern(3000, 11)
	must(t, ov.Write(ino, 0, 0, planned))
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("no plan")
	}
	must(t, ov.Truncate(ino, 0, 500))
	must(t, ov.Write(ino, 0, 2000, []byte("fresh")))
	want := make([]byte, 2005)
	copy(want, planned[:500])
	copy(want[2000:], "fresh")
	if got := readFrom(t, ov, ino, 0, baseOf(nil), 2100); !bytes.Equal(got, want) {
		t.Fatalf("during flush: differs at %d (len %d)", firstDiff(got, want), len(got))
	}
	// the commit carries the whole plan; minSize hides the bytes beyond 500 afterwards
	must(t, ov.CommitFlushed(plan, 3000))
	if got := readFrom(t, ov, ino, 3000, baseOf(planned), 2100); !bytes.Equal(got, want) {
		t.Fatalf("after commit: differs at %d (len %d)", firstDiff(got, want), len(got))
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if got := readFrom(t, ov2, ino, 3000, baseOf(planned), 2100); !bytes.Equal(got, want) {
		t.Fatalf("after restart: differs at %d (len %d)", firstDiff(got, want), len(got))
	}
}

// A truncate that raises the size while a flush plan is outstanding is acknowledged
// (SETATTR) and must survive the compaction that follows the commit and a restart.
// Invariant: I-7, I-37
// Lifecycle: L-172, L-180
func TestOverlay_TruncateUpDuringFlush_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(31)
	data := pattern(1000, 9)
	must(t, ov.Write(ino, 0, 0, data))
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("no plan")
	}
	must(t, ov.Truncate(ino, 0, 5000)) // raised while the flush is in progress
	must(t, ov.CommitFlushed(plan, 1000))
	want := append(bytes.Clone(data), make([]byte, 4000)...)
	if sz, ok := ov.Size(ino, 1000); !ok || sz != 5000 {
		t.Fatalf("Size before restart = %d,%v want 5000", sz, ok)
	}
	if got := readFrom(t, ov, ino, 1000, baseOf(data), 5010); !bytes.Equal(got, want) {
		t.Fatalf("before restart differs at %d (len %d)", firstDiff(got, want), len(got))
	}
	must(t, ov.Close())
	ov2, _ := newOverlay(t, dir)
	if sz, ok := ov2.Size(ino, 1000); !ok || sz != 5000 {
		t.Fatalf("Size after restart = %d,%v want 5000", sz, ok)
	}
	if got := readFrom(t, ov2, ino, 1000, baseOf(data), 5010); !bytes.Equal(got, want) {
		t.Fatalf("after restart differs at %d (len %d)", firstDiff(got, want), len(got))
	}
	// a later shrink below the committed size still records the truncation point
	must(t, ov2.Truncate(ino, 1000, 300))
	if ms, ok := ov2.MinSize(ino); !ok || ms != 300 {
		t.Fatalf("MinSize = %d,%v want 300", ms, ok)
	}
}

// An overlay entry must never pin the committed size it first saw: after its flush the
// entry is dropped, and while dirty it treats the size passed by the caller (the current
// committed size) as the base, so another gateway's growth is visible and a truncate
// records the requested point rather than the stale base.
// Invariant: I-35
// Lifecycle: L-178
func TestOverlay_StaleEntry_FollowsCommittedSize(t *testing.T) {
	ov, _ := newOverlay(t, t.TempDir())
	ino := meta.Ino(9)
	grown := pattern(300, 7) // what another gateway committed meanwhile

	// clean entry left behind by a flush at size 100
	must(t, ov.Write(ino, 100, 0, []byte("0123456789")))
	plan, ok := ov.SnapshotForFlush(ino)
	if !ok {
		t.Fatal("no plan")
	}
	must(t, ov.CommitFlushed(plan, 100))
	if _, ok := ov.Size(ino, 100); ok {
		t.Fatal("a clean entry must not report a size (it is dropped after its flush)")
	}
	// the file grew to 300 through another gateway; this gateway writes 5 bytes at 0
	must(t, ov.Write(ino, 300, 0, []byte("abcde")))
	if sz, ok := ov.Size(ino, 300); !ok || sz != 300 {
		t.Fatalf("Size = %d,%v want 300 (committed size, no truncate)", sz, ok)
	}
	buf := make([]byte, 50)
	n, err := ov.Read(ino, 300, 200, buf, baseOf(grown))
	if err != nil || n != 50 || !bytes.Equal(buf, grown[200:250]) {
		t.Fatalf("read above the stale base: n=%d err=%v differs at %d", n, err, firstDiff(buf, grown[200:250]))
	}
	plan, ok = ov.SnapshotForFlush(ino)
	if !ok || plan.SizeSet || plan.sizeAgainst(300) != 300 {
		t.Fatalf("plan = %+v (size against committed 300 must be 300)", plan)
	}
	ov.AbortFlush(plan)
	// a truncate records the requested point, not the stale base
	must(t, ov.Truncate(ino, 300, 250))
	plan, ok = ov.SnapshotForFlush(ino)
	if !ok || !plan.SizeSet || plan.MinSize != 250 || plan.Size != 250 {
		t.Fatalf("plan after truncate = %+v want MinSize 250", plan)
	}
	n, err = ov.Read(ino, 300, 200, buf, baseOf(grown))
	if err != nil || n != 50 || !bytes.Equal(buf, grown[200:250]) {
		t.Fatalf("read below the truncate point: n=%d err=%v", n, err)
	}
	if n, err = ov.Read(ino, 300, 250, buf, baseOf(grown)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at the truncate point: n=%d err=%v want EOF", n, err)
	}
	// the dirty entry also follows a shrink by another gateway: committed 120, no truncate here
	must(t, ov.CommitFlushed(plan, 250))
	must(t, ov.Write(ino, 120, 0, []byte("zz")))
	if sz, _ := ov.Size(ino, 120); sz != 120 {
		t.Fatalf("Size after the other gateway shrank = %d want 120", sz)
	}
}

// A crash during Compact can leave `<ino>.wal.new`; the old log is a superset of it and
// replays correctly, and the temp file is removed on open rather than left forever.
// Invariant: I-37
func TestOverlay_StaleCompactTempRemoved(t *testing.T) {
	dir := t.TempDir()
	ov, _ := newOverlay(t, dir)
	ino := meta.Ino(41)
	must(t, ov.Write(ino, 0, 300, []byte("live")))
	must(t, ov.Close())
	stale := filepath.Join(dir, "41.wal.new")
	must(t, os.WriteFile(stale, []byte("half-written compaction"), 0o600))
	ov2, _ := newOverlay(t, dir)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale compaction temp still present: %v", err)
	}
	want := append(make([]byte, 300), "live"...)
	if got := readFrom(t, ov2, ino, 0, baseOf(nil), 400); !bytes.Equal(got, want) {
		t.Fatalf("differs at %d", firstDiff(got, want))
	}
}
