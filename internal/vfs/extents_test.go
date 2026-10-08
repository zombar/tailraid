package vfs

import (
	"flag"
	"math/rand"
	"testing"
	"time"
)

var seedFlag = flag.Int64("seed", 0, "seed for randomized tests (0 = time-based)")

func seed(t *testing.T) *rand.Rand {
	t.Helper()
	s := *seedFlag
	if s == 0 {
		s = time.Now().UnixNano()
	}
	t.Logf("seed=%d (re-run with -seed=%d)", s, s)
	return rand.New(rand.NewSource(s))
}

// oracle is a byte-array model of a dirty file: each byte records which write (by sequence
// number) last touched it, 0 meaning "base".
type oracle struct {
	owner []uint32
}

func (o *oracle) write(seq uint32, off, n uint64) {
	if end := off + n; end > uint64(len(o.owner)) {
		o.owner = append(o.owner, make([]uint32, end-uint64(len(o.owner)))...)
	}
	for i := off; i < off+n; i++ {
		o.owner[i] = seq
	}
}

func (o *oracle) truncate(size uint64) {
	if size < uint64(len(o.owner)) {
		o.owner = o.owner[:size]
	}
}

// walOff encodes (seq, offset-within-write) so that split extents can be checked exactly:
// an extent covering byte i must have walOff == seqBase(seq) + (i - origOff).
func checkAgainstOracle(t *testing.T, e extents, o *oracle, origOff map[uint32]uint64) {
	t.Helper()
	// sorted, non-overlapping, n > 0
	for i := range e {
		if e[i].n == 0 {
			t.Fatalf("extent %d has n == 0: %+v", i, e)
		}
		if i > 0 && e[i-1].off+e[i-1].n > e[i].off {
			t.Fatalf("extents overlap or unsorted at %d: %+v", i, e)
		}
	}
	covered := make(map[uint64]extent)
	for _, x := range e {
		for i := x.off; i < x.off+x.n; i++ {
			covered[i] = x
		}
	}
	for i, seq := range o.owner {
		x, ok := covered[uint64(i)]
		if seq == 0 {
			if ok {
				t.Fatalf("byte %d should be base but is covered by %+v", i, x)
			}
			continue
		}
		if !ok {
			t.Fatalf("byte %d should be owned by write %d but is uncovered", i, seq)
		}
		want := int64(seq)<<32 + int64(uint64(i)-origOff[seq])
		got := x.walOff + int64(uint64(i)-x.off)
		if got != want {
			t.Fatalf("byte %d: walOff mismatch got %d want %d (extent %+v)", i, got, want, x)
		}
	}
	for i := range covered {
		if i >= uint64(len(o.owner)) {
			t.Fatalf("byte %d covered beyond oracle size %d", i, len(o.owner))
		}
	}
}

// Invariant: I-34
// Lifecycle: L-6, L-12
func TestExtents_InsertOverlap_Property(t *testing.T) {
	rng := seed(t)
	iters := 300
	if testing.Short() {
		iters = 100
	}
	for it := 0; it < iters; it++ {
		var e extents
		o := &oracle{}
		origOff := map[uint32]uint64{}
		ops := 1 + rng.Intn(60)
		for seq := uint32(1); seq <= uint32(ops); seq++ {
			if rng.Intn(8) == 0 {
				size := uint64(rng.Intn(len(o.owner) + 4))
				e.truncate(size)
				o.truncate(size)
				continue
			}
			off := uint64(rng.Intn(200))
			n := uint64(1 + rng.Intn(50))
			origOff[seq] = off
			e.insert(extent{off: off, n: n, walOff: int64(seq) << 32})
			o.write(seq, off, n)
		}
		checkAgainstOracle(t, e, o, origOff)
		if e.bytes() != countOwned(o) {
			t.Fatalf("bytes() = %d, oracle %d", e.bytes(), countOwned(o))
		}
	}
}

func countOwned(o *oracle) uint64 {
	var n uint64
	for _, s := range o.owner {
		if s != 0 {
			n++
		}
	}
	return n
}

// Lifecycle: L-12
func TestExtents_Truncate(t *testing.T) {
	var e extents
	e.insert(extent{off: 0, n: 10, walOff: 100})
	e.insert(extent{off: 20, n: 10, walOff: 200})
	e.insert(extent{off: 40, n: 10, walOff: 300})
	e.truncate(25)
	if len(e) != 2 || e[1].off != 20 || e[1].n != 5 || e[1].walOff != 200 {
		t.Fatalf("truncate(25): %+v", e)
	}
	e.truncate(10)
	if len(e) != 1 || e[0].n != 10 {
		t.Fatalf("truncate(10): %+v", e)
	}
	e.truncate(0)
	if len(e) != 0 {
		t.Fatalf("truncate(0): %+v", e)
	}
	if e.maxEnd() != 0 {
		t.Fatalf("maxEnd of empty = %d", e.maxEnd())
	}
}

func TestExtents_MaxEndAndOverlapping(t *testing.T) {
	var e extents
	e.insert(extent{off: 5, n: 5, walOff: 1})
	e.insert(extent{off: 30, n: 10, walOff: 2})
	if e.maxEnd() != 40 {
		t.Fatalf("maxEnd = %d", e.maxEnd())
	}
	got := e.overlapping(8, 25) // [8,33)
	if len(got) != 2 || got[0].off != 5 || got[1].off != 30 {
		t.Fatalf("overlapping = %+v", got)
	}
	if got := e.overlapping(10, 20); len(got) != 0 {
		t.Fatalf("overlapping gap = %+v", got)
	}
}

// Lifecycle: L-7
func TestExtents_CoversChunk(t *testing.T) {
	const cs = uint32(100)
	cases := []struct {
		name string
		ext  []extent
		idx  uint64
		size uint64
		want bool
	}{
		{"full aligned", []extent{{off: 100, n: 100}}, 1, 1000, true},
		{"two pieces contiguous", []extent{{off: 100, n: 40}, {off: 140, n: 60}}, 1, 1000, true},
		{"gap", []extent{{off: 100, n: 40}, {off: 150, n: 50}}, 1, 1000, false},
		{"partial", []extent{{off: 100, n: 99}}, 1, 1000, false},
		{"tail chunk short, covered", []extent{{off: 200, n: 30}}, 2, 230, true},
		{"tail chunk short, not covered", []extent{{off: 200, n: 20}}, 2, 230, false},
		{"overhang covers", []extent{{off: 50, n: 200}}, 1, 1000, true},
		{"empty", nil, 0, 100, false},
		{"zero-length chunk at eof", []extent{{off: 0, n: 100}}, 1, 100, false},
	}
	for _, c := range cases {
		var e extents
		for _, x := range c.ext {
			e.insert(x)
		}
		if got := e.coversChunk(c.idx, cs, c.size); got != c.want {
			t.Errorf("%s: coversChunk = %v want %v", c.name, got, c.want)
		}
	}
}
