package stats

import (
	"context"
	"errors"
	"flag"
	"math/rand"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

var seed = flag.Int64("seed", 0, "seed of the randomized tests (0: the clock)")

// versioned wraps memVolume with a version per write.
type versioned struct {
	*memVolume
	versions map[string]int
}

func newVersioned() *versioned { return &versioned{memVolume: newMem(), versions: map[string]int{}} }

func (v *versioned) WriteFile(ctx context.Context, name string, data []byte) error {
	if err := v.memVolume.WriteFile(ctx, name, data); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.versions[name]++
	return nil
}

func (v *versioned) Version(_ context.Context, name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.files[name]; !ok {
		return "", errors.New("no such segment")
	}
	return time.Duration(v.versions[name]).String(), nil
}

// fill writes one row per step for [from, to) at the tier, under writer.
func fill(t *testing.T, vol Volume, tier Tier, writer string, from, to time.Time, step time.Duration) {
	t.Helper()
	ctx := context.Background()
	var rows []Row
	flush := func(first time.Time) {
		if len(rows) == 0 {
			return
		}
		var buf bytesBuffer
		if err := Encode(&buf, rows); err != nil {
			t.Fatal(err)
		}
		if err := vol.WriteFile(ctx, path.Join(Root, tier.Dir(), writer, SegmentName(tier, first, writer)), buf.Bytes()); err != nil {
			t.Fatal(err)
		}
		rows = nil
	}
	var first time.Time
	var end time.Time
	for at := from; at.Before(to); at = at.Add(step) {
		s, e := tier.Period(at)
		if !s.Equal(first) || at.Equal(from) {
			flush(first)
			first, end = s, e
		}
		_ = end
		if writer == ClusterWriter {
			rows = append(rows, Row{T: at.Unix(), Term: 1, By: "a", C: &Cluster{Chunks: 1, Live: map[string]int{"a": 1}}})
		} else {
			rows = append(rows, Row{T: at.Unix(), N: writer, M: map[string]float64{"cpu": 0.5}})
		}
	}
	flush(first)
}

// The finest tier whose retention covers the start of the range answers: the boundary
// table on the range's age.
// Invariant: I-196
func TestStatsQuery_TierChoice_Table(t *testing.T) {
	q := &Query{Keep: DefaultKeep}
	now := utc(2026, 10, 11, 12, 0)
	for _, tc := range []struct {
		age  time.Duration
		want Tier
	}{
		{0, Raw},
		{time.Hour, Raw},
		{48*time.Hour - time.Second, Raw},
		{48 * time.Hour, Raw},
		{48*time.Hour + time.Second, Minute},
		{720*time.Hour - time.Second, Minute},
		{720 * time.Hour, Minute},
		{720*time.Hour + time.Second, Coarse},
		{17520 * time.Hour, Coarse},
		{30000 * time.Hour, Coarse},
	} {
		if got := q.TierFor(now.Add(-tc.age), now); got != tc.want {
			t.Errorf("age %v: tier %v, want %v", tc.age, got, tc.want)
		}
	}
}

// Only segments named within an hour of the range are opened and only rows whose own t
// falls inside it are kept: a name is never trusted over a row (clock skew).
// Invariant: I-196
func TestStatsQuery_NameWindowAndRowFilter(t *testing.T) {
	ctx := context.Background()
	vol := newVersioned()
	day := utc(2026, 10, 11, 0, 0)
	fill(t, vol, Raw, "pike", day, day.Add(24*time.Hour), 10*time.Second)
	// a segment named 13:00 whose rows sit at 14:30 (a skewed writer): found by the window
	var buf bytesBuffer
	_ = Encode(&buf, []Row{{T: day.Add(14*time.Hour + 30*time.Minute).Unix(), N: "skewed", M: map[string]float64{"x": 1}}})
	_ = vol.WriteFile(ctx, path.Join(Root, "10s", "skewed", SegmentName(Raw, day.Add(13*time.Hour), "skewed")), buf.Bytes())
	// a segment named 11:00 whose rows sit at 14:30: outside the window, not opened
	_ = vol.WriteFile(ctx, path.Join(Root, "10s", "far", SegmentName(Raw, day.Add(11*time.Hour), "far")), buf.Bytes())
	q := &Query{Vol: vol, Clk: clock.NewFake(day.Add(24 * time.Hour)), Keep: DefaultKeep, CacheBytes: 32 << 20}
	rows, rg, err := q.Rows(ctx, day.Add(14*time.Hour), day.Add(15*time.Hour), false)
	if err != nil || rg.Tier != Raw {
		t.Fatalf("%v %+v", err, rg)
	}
	pike, skewed, far := 0, 0, 0
	for _, r := range rows {
		switch r.N {
		case "pike":
			pike++
			if r.T < day.Add(14*time.Hour).Unix() || r.T >= day.Add(15*time.Hour).Unix() {
				t.Fatalf("row outside the range: %d", r.T)
			}
		case "skewed":
			skewed++
		case "far":
			far++
		}
	}
	if pike != 360 || skewed != 1 || far != 0 {
		t.Fatalf("pike %d skewed %d far %d", pike, skewed, far)
	}
}

// The parsed-segment cache never exceeds its bound, drops the least recently used first,
// and re-reads a segment whose version changed: the boundary table on the bound.
// Invariant: I-196
func TestStatsQuery_CacheBound_Table(t *testing.T) {
	ctx := context.Background()
	vol := newVersioned()
	day := utc(2026, 10, 11, 0, 0)
	fill(t, vol, Raw, "pike", day, day.Add(6*time.Hour), 10*time.Second)
	names, _ := vol.List(ctx, path.Join(Root, "10s", "pike"))
	var smallest, sum int64
	for _, n := range names {
		size := int64(len(vol.files[path.Join(Root, "10s", "pike", n)])) * 8 // the cache's estimate of a parsed segment
		if smallest == 0 || size < smallest {
			smallest = size
		}
		sum += size
	}
	for _, tc := range []struct {
		bound          int64
		some, everyone bool
	}{{0, false, false}, {1, false, false}, {smallest - 1, false, false}, {smallest, true, false}, {smallest + 1, true, false}, {sum - 1, true, false}, {sum, true, true}, {sum + 1, true, true}} {
		q := &Query{Vol: vol, Clk: clock.NewFake(day.Add(6 * time.Hour)), Keep: DefaultKeep, CacheBytes: tc.bound}
		if _, _, err := q.Rows(ctx, day, day.Add(6*time.Hour), false); err != nil {
			t.Fatal(err)
		}
		c := q.Cached()
		if c > tc.bound || (tc.some && c == 0) || (!tc.some && c != 0) || (tc.everyone && c != sum) || (!tc.everyone && c == sum) {
			t.Errorf("bound %d: cached %d (smallest %d, sum %d)", tc.bound, c, smallest, sum)
		}
	}
	// a version change re-reads: the month file rebuilt daily
	q := &Query{Vol: vol, Clk: clock.NewFake(day.Add(6 * time.Hour)), Keep: DefaultKeep, CacheBytes: 100 * sum}
	reads := func() int { vol.mu.Lock(); defer vol.mu.Unlock(); return vol.writes }
	_, _, _ = q.Rows(ctx, day, day.Add(6*time.Hour), false)
	before := reads()
	_, _, _ = q.Rows(ctx, day, day.Add(6*time.Hour), false)
	if reads() != before {
		t.Fatal("a cached query touched the volume")
	}
	p := path.Join(Root, "10s", "pike", names[0])
	data, _ := vol.ReadFile(ctx, p)
	_ = vol.WriteFile(ctx, p, data) // same bytes, new version
	rows, _, _ := q.Rows(ctx, day, day.Add(6*time.Hour), false)
	if len(rows) != 6*360 {
		t.Fatalf("after a rewrite: %d rows", len(rows))
	}
}

// The live edge asks every live member but self for its rows since the newest instant
// it reported, within one fan-out every two seconds shared by every caller; a member
// that fails leaves a gap (no row, never a zero) and is not asked again for 30 s.
// Invariant: I-196
func TestStatsQuery_FanOut_TimeoutIsGap_BackOff(t *testing.T) {
	ctx := context.Background()
	now := utc(2026, 10, 11, 12, 0)
	clk := clock.NewFake(now)
	var calls atomic.Int32
	var sinceB atomic.Int64
	fetch := func(_ context.Context, addr string, since int64) ([]Row, error) {
		calls.Add(1)
		switch addr {
		case "b:1":
			sinceB.Store(since)
			return []Row{{T: since + 1, N: "b", M: map[string]float64{"cpu": 1}}, {T: since + 2, N: "b", M: map[string]float64{"cpu": 1}}}, nil
		case "c:1":
			return nil, errors.New("timeout")
		}
		return nil, nil
	}
	members := func() []Member {
		return []Member{{Name: "a", Addr: "a:1", Live: true, Self: true}, {Name: "b", Addr: "b:1", Live: true}, {Name: "c", Addr: "c:1", Live: true}, {Name: "d", Addr: "d:1", Live: false}}
	}
	q := &Query{
		Vol: newVersioned(), Clk: clk, Keep: DefaultKeep, Members: members, Fetch: fetch,
		Self: func(since int64) []Row { return []Row{{T: now.Unix(), N: "a", M: map[string]float64{"cpu": 0}}} },
	}
	from := now.Add(-time.Hour)
	rows, _, err := q.Rows(ctx, from, now.Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 { // b and c; never self, never the dead d
		t.Fatalf("%d fetches, want 2", calls.Load())
	}
	if sinceB.Load() != from.Unix()-1 {
		t.Fatalf("first since %d, want the range start", sinceB.Load())
	}
	names := map[string]int{}
	for _, r := range rows {
		names[r.N]++
	}
	if names["a"] != 1 || names["b"] != 2 || names["c"] != 0 {
		t.Fatalf("rows per member %v", names)
	}
	// within the share window: no fetch at all, the same rows
	if _, _, _ = q.Rows(ctx, from, now.Add(time.Minute), true); calls.Load() != 2 {
		t.Fatalf("a second caller within %v fetched again", FetchShare)
	}
	// after it: b is asked since its newest row, c is in back-off
	clk.Advance(FetchShare)
	_, _, _ = q.Rows(ctx, from, now.Add(time.Minute), true)
	if calls.Load() != 3 || sinceB.Load() != from.Unix()+1 {
		t.Fatalf("calls %d since %d after the share window", calls.Load(), sinceB.Load())
	}
	clk.Advance(FetchBackoff)
	_, _, _ = q.Rows(ctx, from, now.Add(time.Minute), true)
	if calls.Load() != 5 {
		t.Fatalf("calls %d after the back-off, want c asked again", calls.Load())
	}
	if !q.Asking() {
		t.Fatal("a browser just asked")
	}
	clk.Advance(FetchIdle)
	if q.Asking() {
		t.Fatal("nobody asked for ten seconds")
	}
}

// Rows are deduplicated by (node, t); cluster rows by t keeping the highest term; the
// order is by t then node, whatever the input order (a seeded shuffle).
// Invariant: I-196
func TestStatsQuery_Dedup_TermRule_Seeded(t *testing.T) {
	s := *seed
	if s == 0 {
		s = time.Now().UnixNano()
	}
	t.Logf("seed %d", s)
	rng := rand.New(rand.NewSource(s)) //nolint:gosec // a seeded test
	var rows []Row
	for i := range 100 {
		rows = append(rows,
			Row{T: int64(i), N: "a", M: map[string]float64{"v": 1}},
			Row{T: int64(i), N: "a", M: map[string]float64{"v": 2}}, // a duplicate from the archive and the live edge
			Row{T: int64(i), N: "b", M: map[string]float64{"v": 3}},
			Row{T: int64(i), Term: 5, By: "old", C: &Cluster{Chunks: 5}},
			Row{T: int64(i), Term: 6, By: "new", C: &Cluster{Chunks: 6}})
	}
	rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	out := dedup(rows)
	if len(out) != 300 {
		t.Fatalf("%d rows, want 300", len(out))
	}
	for i, r := range out {
		if i > 0 && (r.T < out[i-1].T || (r.T == out[i-1].T && r.N < out[i-1].N)) {
			t.Fatalf("not ordered at %d: %+v after %+v", i, r, out[i-1])
		}
		if r.C != nil && (r.Term != 6 || r.By != "new" || r.C.Chunks != 6) {
			t.Fatalf("the lower term won: %+v", r)
		}
	}
}

// bytesBuffer is bytes.Buffer without the import collision in this file.
type bytesBuffer struct{ b []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }
func (b *bytesBuffer) Bytes() []byte               { return b.b }
