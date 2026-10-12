package repair_test

import (
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

// phased is a hash whose scrub phase in a period of p is phase.
func phased(phase time.Duration) meta.Hash {
	var h meta.Hash
	binary.BigEndian.PutUint64(h[:8], uint64(phase))
	return h
}

// A chunk is due in a pass when its phase lies in the arc (from mod P, to mod P]: windows
// {0, 1ns, P−1, P, P+1} around the phase, an arc that wraps past P, and P = 0 (off).
// Invariant: I-215
func TestScrubDue_Window(t *testing.T) {
	const p = time.Hour
	base := time.Unix(1_800_000_000, 0).Truncate(p) // a multiple of P: base mod P = 0
	phi := 20 * time.Minute
	for _, c := range []struct {
		name     string
		from, to time.Duration // offsets from base
		phase    time.Duration
		period   time.Duration
		due      bool
	}{
		{"empty window", phi, phi, phi, p, false},
		{"1ns ending on the phase", phi - 1, phi, phi, p, true},
		{"1ns starting on the phase", phi, phi + 1, phi, p, false},
		{"P-1 starting on the phase", phi, phi + p - 1, phi, p, false},
		{"P-1 starting before the phase", phi - 1, phi + p - 2, phi, p, true},
		{"P", phi, phi + p, phi, p, true},
		{"P+1", phi, phi + p + 1, phi, p, true},
		{"wrap, phase after the start", p - 10, p + 10, p - 5, p, true},
		{"wrap, phase after the end", p - 10, p + 10, 5, p, true},
		{"wrap, phase outside", p - 10, p + 10, p - 20, p, false},
		{"window ending past one period", 2*p + phi - 1, 2*p + phi, phi, p, true},
		{"off", 0, 10 * p, phi, 0, false},
		{"backwards", phi + 1, phi - 1, phi, p, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := repair.ScrubDue(phased(c.phase), base.Add(c.from), base.Add(c.to), c.period)
			if got != c.due {
				t.Fatalf("due %v, want %v", got, c.due)
			}
		})
	}
}

// writeChunks flushes n one-chunk files and returns their hashes.
func writeChunks(t *testing.T, f *testharness.Fabric, n int) []meta.Hash {
	t.Helper()
	fl := f.Flusher()
	var out []meta.Hash
	for i := range n {
		ino := f.Create("s" + strconv.Itoa(i))
		if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), byte(60+i))); err != nil {
			t.Fatal(err)
		}
		if err := fl.FlushInode(ctx, ino); err != nil {
			t.Fatal(err)
		}
		out = append(out, f.Inode(ino).Chunks[0])
	}
	return out
}

func scrubCfg(period time.Duration) repair.Config {
	cfg := repair.DefaultConfig()
	cfg.ScrubPeriod, cfg.SumsBatch, cfg.RateBytes = period, 0, 0
	return cfg
}

// Passes ScanInterval apart that span one period scrub every chunk exactly once, however
// the period divides into passes; a pass at P = 0 scrubs nothing, and `check --scrub`
// scrubs everything whatever P.
// Invariant: I-215
func TestRepair_ScrubPeriod_CoversEveryChunkOnce(t *testing.T) {
	const n = 24
	f := testharness.NewFabric(t, 3, 2, 1)
	writeChunks(t, f, n)
	for _, passes := range []int{1, 3, 10} {
		t.Run(strconv.Itoa(passes), func(t *testing.T) {
			cfg := scrubCfg(0)
			cfg.ScrubPeriod = time.Duration(passes) * cfg.ScanInterval
			r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
			total := 0
			for range passes {
				st, err := r.Scan(ctx)
				if err != nil {
					t.Fatal(err)
				}
				total += st.Scrubbed
				f.Clk.Advance(cfg.ScanInterval)
			}
			if total != n {
				t.Fatalf("%d passes over one period scrubbed %d chunks, want %d", passes, total, n)
			}
		})
	}
	off := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, scrubCfg(0), nil)
	if st, err := off.Scan(ctx); err != nil || st.Scrubbed != 0 {
		t.Fatalf("scrub_period 0: scrubbed %d, %v; want 0", st.Scrubbed, err)
	}
	if st, err := off.Scrub(ctx); err != nil || st.Scrubbed != n {
		t.Fatalf("check --scrub: scrubbed %d, %v; want %d", st.Scrubbed, err, n)
	}
}

// A repairer's first pass, and its first pass after a stretch without leadership, covers
// the ScanInterval before it, not the time since its last pass: a node that leads again
// after a day does not scrub a day's share at once.
// Invariant: I-215
func TestRepair_ScrubPeriod_FirstPassAsLeader(t *testing.T) {
	const n = 40
	f := testharness.NewFabric(t, 3, 2, 1)
	hashes := writeChunks(t, f, n)
	cfg := scrubCfg(4 * time.Minute) // a ScanInterval (1 min) is a quarter of the period
	want := func() int {
		now := f.Clk.Now()
		due := 0
		for _, h := range hashes {
			if repair.ScrubDue(h, now.Add(-cfg.ScanInterval), now, cfg.ScrubPeriod) {
				due++
			}
		}
		return due
	}
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
	w := want()
	if st, err := r.Scan(ctx); err != nil || st.Scrubbed != w || w == n {
		t.Fatalf("first pass scrubbed %d, %v; want %d of %d", st.Scrubbed, err, w, n)
	}
	r.LostLeadership()
	f.Clk.Advance(24 * time.Hour)
	w = want()
	if st, err := r.Scan(ctx); err != nil || st.Scrubbed != w || w == n {
		t.Fatalf("first pass as leader again scrubbed %d, %v; want %d of %d", st.Scrubbed, err, w, n)
	}
}
