package stats

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/clock"
)

// memVolume is an in-memory Volume: whole files by path, with an optional refusal.
type memVolume struct {
	mu     sync.Mutex
	files  map[string][]byte
	refuse error
	writes int
}

func newMem() *memVolume { return &memVolume{files: map[string][]byte{}} }

func (m *memVolume) WriteFile(_ context.Context, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refuse != nil {
		return m.refuse
	}
	m.files[name] = append([]byte(nil), data...)
	m.writes++
	return nil
}

func (m *memVolume) ReadFile(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}

func (m *memVolume) List(_ context.Context, dir string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for p := range m.files {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, "/")
		if !seen[first] {
			seen[first] = true
			out = append(out, first)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memVolume) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[name]; !ok {
		return os.ErrNotExist
	}
	delete(m.files, name)
	return nil
}

func (m *memVolume) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for p := range m.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (m *memVolume) rows(t *testing.T, name string) []Row {
	t.Helper()
	b, err := m.ReadFile(context.Background(), name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	rows, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return rows
}

func nodeRowAt(at time.Time) Row {
	return Row{T: at.Unix(), N: "pike", M: map[string]float64{"cpu": 0.1}}
}

func clusterRowAt(at time.Time, term uint64) Row {
	return Row{T: at.Unix(), Term: term, By: "pike", C: &Cluster{Chunks: 1, Live: map[string]int{"pike": 1}}}
}

// A seal writes the rows before the boundary as one raw segment per writer under the
// writer's own directory, named by the first row's hour, or its minute for a partial
// hour; the rows after the boundary stay; the second seal of one minute takes the seconds
// form rather than overwriting.
// Invariant: I-192
func TestWriter_SealsAtBoundary_NamesPartialHours(t *testing.T) {
	ctx := context.Background()
	vol := newMem()
	w := &Writer{Vol: vol, Name: "pike"}
	h := utc(2026, 10, 11, 14, 0)
	for s := 5; s < 3600+30; s += 10 {
		w.Add(nodeRowAt(h.Add(time.Duration(s) * time.Second)))
	}
	w.Add(clusterRowAt(h.Add(20*time.Second), 3))
	if err := w.Seal(ctx, h.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	want := []string{Root + "/10s/cluster/2026-10-11T14-cluster.jsonl.gz", Root + "/10s/pike/2026-10-11T14-pike.jsonl.gz"}
	if got := vol.names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("segments %v, want %v", got, want)
	}
	if rows := vol.rows(t, want[1]); len(rows) != 360 || rows[0].T != h.Add(5*time.Second).Unix() || rows[359].T >= h.Add(time.Hour).Unix() {
		t.Fatalf("the hour's rows: %d, first %d", len(rows), rows[0].T)
	}
	if w.Unsealed() != 3 {
		t.Fatalf("%d rows left unsealed, want 3", w.Unsealed())
	}
	// a stop at 15:00:35 seals the partial hour; its first row is in minute zero, so it
	// carries the hour's name (the same period either way)
	if err := w.Seal(ctx, h.Add(time.Hour+35*time.Second)); err != nil {
		t.Fatal(err)
	}
	partial := Root + "/10s/pike/2026-10-11T15-pike.jsonl.gz"
	if _, err := vol.ReadFile(ctx, partial); err != nil {
		t.Fatalf("partial hour: %v (%v)", err, vol.names())
	}
	// rows from 15:23:15 on, sealed at 16:00: named by their minute
	for s := 23*60 + 15; s < 3600; s += 10 {
		w.Add(nodeRowAt(h.Add(time.Hour + time.Duration(s)*time.Second)))
	}
	if err := w.Seal(ctx, h.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := vol.ReadFile(ctx, Root+"/10s/pike/2026-10-11T1523-pike.jsonl.gz"); err != nil {
		t.Fatalf("late start: %v (%v)", err, vol.names())
	}
	// two seals within one minute: the second takes the seconds form
	w.Add(nodeRowAt(h.Add(2*time.Hour + 7*60*time.Second + 5*time.Second)))
	_ = w.Seal(ctx, h.Add(2*time.Hour+7*60*time.Second+10*time.Second))
	w.Add(nodeRowAt(h.Add(2*time.Hour + 7*60*time.Second + 15*time.Second)))
	_ = w.Seal(ctx, h.Add(2*time.Hour+7*60*time.Second+20*time.Second))
	a, b := Root+"/10s/pike/2026-10-11T1607-pike.jsonl.gz", Root+"/10s/pike/2026-10-11T160715-pike.jsonl.gz"
	if _, err := vol.ReadFile(ctx, a); err != nil {
		t.Fatalf("first seal of the minute: %v", err)
	}
	if _, err := vol.ReadFile(ctx, b); err != nil {
		t.Fatalf("second seal of the minute: %v (%v)", err, vol.names())
	}
	if p, ok := ParseName(Raw, path.Base(b)); !ok || !p.Start.Equal(utc(2026, 10, 11, 16, 7).Add(15*time.Second)) {
		t.Fatalf("the seconds form parses: %+v %v", p, ok)
	}
	// an empty seal writes nothing
	n := vol.writes
	if err := w.Seal(ctx, h.Add(3*time.Hour)); err != nil || vol.writes != n {
		t.Fatalf("empty seal wrote: %v %d", err, vol.writes-n)
	}
}

// A seal the cluster cannot take is spilled to the local directory and written at the
// next start; without a spill directory the rows wait for the next seal.
// Invariant: I-192
func TestWriter_SpillAndRecover(t *testing.T) {
	ctx := context.Background()
	vol := newMem()
	vol.refuse = errors.New("EROFS")
	spill := t.TempDir()
	w := &Writer{Vol: vol, Name: "pike", Spill: spill}
	h := utc(2026, 10, 11, 14, 0)
	w.Add(nodeRowAt(h.Add(5 * time.Second)))
	w.Add(clusterRowAt(h.Add(5*time.Second), 2))
	if err := w.Seal(ctx, h.Add(time.Hour)); err == nil {
		t.Fatal("a refused seal reported no error")
	}
	if w.Unsealed() != 0 {
		t.Fatalf("spilled rows still unsealed: %d", w.Unsealed())
	}
	for _, p := range []string{"pike/2026-10-11T14-pike.jsonl.gz", "cluster/2026-10-11T14-cluster.jsonl.gz"} {
		if _, err := os.Stat(filepath.Join(spill, p)); err != nil {
			t.Fatalf("spill %s: %v", p, err)
		}
	}
	// still refused at the next start: the spill stays
	if err := w.Recover(ctx); err == nil {
		t.Fatal("recover against a refusing volume reported no error")
	}
	vol.refuse = nil
	if err := w.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := vol.names(); len(got) != 2 {
		t.Fatalf("recovered %v", got)
	}
	if rows := vol.rows(t, Root+"/10s/pike/2026-10-11T14-pike.jsonl.gz"); len(rows) != 1 || rows[0].T != h.Add(5*time.Second).Unix() {
		t.Fatalf("recovered rows %+v", rows)
	}
	if left, _ := os.ReadDir(filepath.Join(spill, "pike")); len(left) != 0 {
		t.Fatalf("spill not cleared: %v", left)
	}
	// no spill directory: the rows go back
	w2 := &Writer{Vol: &memVolume{files: map[string][]byte{}, refuse: errors.New("no leader")}, Name: "pike"}
	w2.Add(nodeRowAt(h))
	if err := w2.Seal(ctx, h.Add(time.Hour)); err == nil || w2.Unsealed() != 1 {
		t.Fatalf("rows not given back: %v %d", err, w2.Unsealed())
	}
}

// The day's minute segment is derived from the sealed raw segments read back from the
// volume (nothing from memory), the month segment is rebuilt with the day appended, a
// second rebuild writes identical bytes, and the catch-up rolls up a missing day only
// while its raw rows exist.
// Invariant: I-193
func TestWriter_RollupDay_MonthRebuild_CatchUp(t *testing.T) {
	ctx := context.Background()
	vol := newMem()
	w := &Writer{Vol: vol, Name: "pike"}
	day := utc(2026, 10, 11, 0, 0)
	for hour := range 24 {
		h := day.Add(time.Duration(hour) * time.Hour)
		for s := 0; s < 3600; s += 10 {
			w.Add(nodeRowAt(h.Add(time.Duration(s) * time.Second)))
		}
		if err := w.Seal(ctx, h.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.RollupDay(ctx, "pike", day); err != nil {
		t.Fatal(err)
	}
	minute := Root + "/1m/pike/2026-10-11-pike.jsonl.gz"
	month := Root + "/10m/pike/2026-10-pike.jsonl.gz"
	if rows := vol.rows(t, minute); len(rows) != 1440 || rows[0].T != day.Unix() || rows[1439].T != day.Add(24*time.Hour-time.Minute).Unix() {
		t.Fatalf("minute rows %d", len(rows))
	}
	first, _ := vol.ReadFile(ctx, month)
	if rows := vol.rows(t, month); len(rows) != 144 {
		t.Fatalf("coarse rows %d", len(rows))
	}
	// the next day appends to the month; rebuilding the same day again is idempotent
	next := day.AddDate(0, 0, 1)
	for s := 0; s < 3600; s += 10 {
		w.Add(nodeRowAt(next.Add(time.Duration(s) * time.Second)))
	}
	_ = w.Seal(ctx, next.Add(time.Hour))
	if err := w.RollupDay(ctx, "pike", next); err != nil {
		t.Fatal(err)
	}
	if rows := vol.rows(t, month); len(rows) != 144+6 || rows[144].T != next.Unix() {
		t.Fatalf("month after the next day: %d rows", len(rows))
	}
	if err := w.RollupDay(ctx, "pike", next); err != nil {
		t.Fatal(err)
	}
	again, _ := vol.ReadFile(ctx, month)
	if rows := vol.rows(t, month); len(rows) != 150 {
		t.Fatalf("a second rebuild changed the month: %d rows", len(rows))
	}
	_ = first
	if !bytes.Equal(again, func() []byte { _ = w.RollupDay(ctx, "pike", next); b, _ := vol.ReadFile(ctx, month); return b }()) {
		t.Fatal("a rebuild from identical input differs")
	}
	// catch-up: drop the minute segment, the catch-up restores it; a day without raw
	// rows stays a gap; today is never rolled up
	_ = vol.Remove(ctx, minute)
	now := next.Add(time.Hour + 5*time.Minute)
	if err := w.CatchUp(ctx, now, 48*time.Hour, "pike"); err != nil {
		t.Fatal(err)
	}
	if _, err := vol.ReadFile(ctx, minute); err != nil {
		t.Fatalf("catch-up did not restore the minute segment: %v", err)
	}
	if _, err := vol.ReadFile(ctx, Root+"/1m/pike/2026-10-10-pike.jsonl.gz"); err == nil {
		t.Fatal("a day without raw rows was rolled up")
	}
	// today is never rolled up by the catch-up: on a fresh volume with raw rows of today
	// only, it writes nothing
	fresh := &Writer{Vol: newMem(), Name: "pike"}
	fresh.Add(nodeRowAt(next.Add(30 * time.Second)))
	_ = fresh.Seal(ctx, next.Add(time.Hour))
	if err := fresh.CatchUp(ctx, now, 48*time.Hour, "pike"); err != nil {
		t.Fatal(err)
	}
	if names := fresh.Vol.(*memVolume).names(); len(names) != 1 {
		t.Fatalf("today was rolled up: %v", names)
	}
	// a day with no raw rows at all writes nothing
	if err := w.RollupDay(ctx, "pike", utc(2020, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
}

// Retention removes every writer's segments whose period ended more than the tier's keep
// ago, a removed member's included, and nothing younger: the boundary table per tier.
// Invariant: I-194
func TestWriter_Retain_AnyWriter_Table(t *testing.T) {
	ctx := context.Background()
	vol := newMem()
	keep := Keep{Raw: 48 * time.Hour, Minute: 720 * time.Hour, Coarse: 17520 * time.Hour}
	now := utc(2026, 10, 11, 12, 0)
	put := func(tier Tier, writer string, start time.Time) string {
		p := path.Join(Root, tier.Dir(), writer, SegmentName(tier, start, writer))
		_ = vol.WriteFile(ctx, p, []byte("x"))
		return p
	}
	type row struct {
		path string
		gone bool
	}
	var rows []row
	add := func(tier Tier, writer string, age time.Duration, gone bool) {
		var start time.Time
		switch tier {
		case Raw:
			start = now.Add(-age).Truncate(time.Hour).Add(-time.Hour)
		case Minute:
			s, _ := Minute.Period(now.Add(-age).AddDate(0, 0, -1))
			start = s
		case Coarse:
			s, _ := Coarse.Period(now.Add(-age).AddDate(0, -1, 0))
			start = s
		}
		rows = append(rows, row{put(tier, writer, start), gone})
	}
	// raw: a segment ending just inside keep stays, one just past it goes
	add(Raw, "pike", 47*time.Hour, false)
	add(Raw, "gone-member", 49*time.Hour, true)
	add(Raw, "cluster", 100*time.Hour, true)
	add(Minute, "pike", 700*time.Hour, false)
	add(Minute, "gone-member", 721*time.Hour+24*time.Hour, true)
	add(Coarse, "pike", 17000*time.Hour, false)
	add(Coarse, "pike", 17520*time.Hour+31*24*time.Hour, true)
	_ = vol.WriteFile(ctx, Root+"/10s/pike/.2026-01-01T00-pike.jsonl.gz.tailraid-stats", []byte("temp")) // a temp is not a segment
	removed, err := w(vol).Retain(ctx, now, keep)
	if err != nil {
		t.Fatal(err)
	}
	wantGone := 0
	for _, r := range rows {
		_, err := vol.ReadFile(ctx, r.path)
		if r.gone {
			wantGone++
		}
		if gone := err != nil; gone != r.gone {
			t.Errorf("%s: removed %v, want %v", r.path, gone, r.gone)
		}
	}
	if removed != wantGone {
		t.Errorf("removed %d, want %d", removed, wantGone)
	}
	if _, err := vol.ReadFile(ctx, Root+"/10s/pike/.2026-01-01T00-pike.jsonl.gz.tailraid-stats"); err != nil {
		t.Error("the temp was removed")
	}
}

func w(vol Volume) *Writer { return &Writer{Vol: vol, Name: "pike"} }

// The live edge hands out at most MaxLive rows newer than since, oldest first, this
// node's own rows and its cluster rows: the boundary table on the count.
// Invariant: I-199
func TestWriter_Live_SinceAndCap_Table(t *testing.T) {
	h := utc(2026, 10, 11, 14, 0)
	for _, n := range []int{0, 1, MaxLive - 1, MaxLive, MaxLive + 1} {
		w := &Writer{Vol: newMem(), Name: "pike"}
		for i := range n {
			w.Add(nodeRowAt(h.Add(time.Duration(i) * 5 * time.Second)))
		}
		got := w.Live(0)
		if len(got) != min(n, MaxLive) {
			t.Errorf("%d rows: live %d", n, len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i].T < got[i-1].T {
				t.Errorf("%d rows: not oldest first", n)
			}
		}
		if n > MaxLive && got[0].T != h.Add(time.Duration(n-MaxLive)*5*time.Second).Unix() {
			t.Errorf("%d rows: the oldest kept is %d", n, got[0].T)
		}
		if n > 0 {
			since := h.Add(time.Duration(n-1) * 5 * time.Second).Unix()
			if rest := w.Live(since); len(rest) != 0 {
				t.Errorf("%d rows: since the last gives %d", n, len(rest))
			}
			if rest := w.Live(since - 1); len(rest) != 1 {
				t.Errorf("%d rows: since the second-to-last gives %d", n, len(rest))
			}
		}
	}
	// cluster rows are merged by time
	w := &Writer{Vol: newMem(), Name: "pike"}
	w.Add(nodeRowAt(h.Add(10 * time.Second)))
	w.Add(clusterRowAt(h.Add(5*time.Second), 1))
	if got := w.Live(0); len(got) != 2 || got[0].C == nil || got[1].N != "pike" {
		t.Errorf("merged %+v", got)
	}
}

// The service samples on its interval, seals exactly at the UTC hour boundary on the
// fake clock, and rolls up at the first boundary of a day; the leader's chunk walk runs
// at most once a minute; a follower writes no cluster rows.
// Invariant: I-191, I-192
func TestService_Run_SealsAtHour(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vol := newMem()
	start := utc(2026, 10, 11, 23, 59).Add(25 * time.Second) // samples at :35, :45, :55; the boundary alone at :00
	clk := clock.NewFake(start)
	walks := 0
	leader := true
	svc := &Service{
		Name: "pike", Every: 10 * time.Second, Keep: DefaultKeep, Clk: clk, Writer: &Writer{Vol: vol, Name: "pike"},
		Src: Sources{
			Machine:  func() *admin.MachineReport { return &admin.MachineReport{Load1: 1} },
			IsLeader: func() bool { return leader },
			Term:     func() uint64 { return 7 },
			Health:   func() admin.VolumeHealth { walks++; return admin.VolumeHealth{Degraded: 1} },
		},
	}
	done := make(chan struct{})
	go func() { defer close(done); svc.Run(ctx) }()
	// 35 s to midnight: three samples, then the boundary seals the partial hour
	waitFor(t, clk)
	for range 3 {
		clk.Advance(10 * time.Second)
		waitFor(t, clk)
	}
	if n := svc.Writer.Unsealed(); n != 6 {
		t.Fatalf("%d unsealed rows before the boundary, want 6 (3 node, 3 cluster)", n)
	}
	clk.Advance(5 * time.Second) // midnight: the hour timer alone
	waitFor(t, clk)
	testEventually(t, func() bool { return svc.Writer.Unsealed() == 0 }, "the boundary seal")
	for _, p := range []string{Root + "/10s/pike/2026-10-11T2359-pike.jsonl.gz", Root + "/10s/cluster/2026-10-11T2359-cluster.jsonl.gz"} {
		if _, err := vol.ReadFile(ctx, p); err != nil {
			t.Fatalf("%s: %v (%v)", p, err, vol.names())
		}
	}
	// the day boundary rolled the day up for both writers
	for _, p := range []string{Root + "/1m/pike/2026-10-11-pike.jsonl.gz", Root + "/10m/cluster/2026-10-cluster.jsonl.gz"} {
		if _, err := vol.ReadFile(ctx, p); err != nil {
			t.Fatalf("%s: %v (%v)", p, err, vol.names())
		}
	}
	// one walk so far; eleven more samples within the minute add none, the twelfth does
	if walks != 1 {
		t.Fatalf("%d walks, want 1", walks)
	}
	for range 6 {
		clk.Advance(10 * time.Second)
		waitFor(t, clk)
	}
	if walks != 2 {
		t.Fatalf("%d walks after a minute, want 2", walks)
	}
	leader = false
	before := svc.Writer.cluster.len()
	clk.Advance(10 * time.Second)
	waitFor(t, clk)
	if svc.Writer.cluster.len() != before {
		t.Fatal("a follower wrote a cluster row")
	}
	cancel()
	<-done
}

// waitFor lets the service's goroutine consume the timer the fake clock just fired and
// re-arm it: the service keeps two timers pending (the sample and the hour).
func waitFor(t *testing.T, clk *clock.Fake) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for clk.Pending() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("no timer re-armed")
		}
		time.Sleep(time.Millisecond) //nolint:forbidigo // the harness's own polling, as testharness.Eventually
	}
}

func testEventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(time.Millisecond) //nolint:forbidigo // as above
	}
}
