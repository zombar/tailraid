package soak

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chart"
)

// A summary with no phases and an abort reason renders as ABORTED with the reason, never
// PASS: zero failed checks over zero phases is not a green run.
//
// Invariant: I-114
func TestRender_ZeroPhasesAborted(t *testing.T) {
	for _, s := range []Summary{
		{Aborted: "soak: status: no route to host"},
		{Aborted: "cancelled after 3 churn: context canceled", Phases: []PhaseResult{{Name: "1 baseline"}, {Name: "3 churn", NotRun: true}}},
		{Aborted: "boom", Failed: 2, Partial: true},
	} {
		d := buildReport(s)
		if d.Verdict != "ABORTED" || d.Aborted != s.Aborted {
			t.Fatalf("verdict %s (%q) for %+v", d.Verdict, d.Aborted, s)
		}
		var out bytes.Buffer
		if err := Render(&out, s); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "ABORTED — ") || !strings.Contains(out.String(), s.Aborted) {
			t.Fatal("report lacks the verdict or the reason")
		}
	}
	if d := buildReport(Summary{}); d.Verdict != "PASS" {
		t.Fatalf("an empty, un-aborted summary is %s", d.Verdict) // the harness test guards the real case
	}
}

// Phases the run never reached are NOT RUN in the matrix: not skipped (that is a
// capability the environment lacks), not passed, no duration.
//
// Invariant: I-114
func TestRender_NotRunPhases(t *testing.T) {
	s := Summary{Aborted: "cancelled", Phases: []PhaseResult{
		{Name: "1 baseline", Checks: []Record{{Kind: "check", Name: "ok", OK: new(true)}}, Took: time.Second},
		{Name: "2 integrity", NotRun: true},
	}}
	d := buildReport(s)
	if d.Phases[1].Status != "NOT RUN" || d.Phases[1].Took != "" || len(d.Skipped) != 0 || d.Checks != 1 {
		t.Fatalf("report: %+v skipped %v checks %d", d.Phases[1], d.Skipped, d.Checks)
	}
	var out bytes.Buffer
	if err := Render(&out, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NOT RUN") {
		t.Fatal("report lacks NOT RUN")
	}
}

// A skipped check is neither a pass nor a fail: it is not counted, it covers nothing, and
// the matrix shows SKIPPED with the reason.
//
// Invariant: I-114
func TestSummary_SkippedCheckCoversNothing(t *testing.T) {
	ph := PhaseResult{Name: "12 large file", Checks: []Record{
		{Kind: "check", Name: "the WAL stayed within its cap", Cites: "I-80", Skip: true, Text: "no sample landed"},
		{Kind: "check", Name: "hash exact", Cites: "I-38", OK: new(true)},
	}}
	cov := coverage([]PhaseResult{ph})
	for _, row := range cov {
		if row.ID == "I-80" {
			t.Fatalf("a skipped check covers I-80: %+v", row)
		}
	}
	s := Summary{Phases: []PhaseResult{ph}, Coverage: cov}
	d := buildReport(s)
	if d.Checks != 1 || d.Failed != 0 || d.Verdict != "PASS" || !d.Phases[0].Checks[0].Skipped || d.Phases[0].Checks[0].OK {
		t.Fatalf("report: checks %d failed %d verdict %s, %+v", d.Checks, d.Failed, d.Verdict, d.Phases[0].Checks[0])
	}
	var out bytes.Buffer
	if err := Render(&out, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "SKIPPED</td><td>no sample landed") {
		t.Fatal("report lacks the SKIPPED cell with its reason")
	}
}

// A chart survives NaN and Inf points (dropped), an empty series and a single point, and
// never writes NaN into a coordinate; long marker labels are cut.
func TestCharts_NaNEmptySingle(t *testing.T) {
	long := strings.Repeat("kill n1 and then some more words ", 4)
	svg := chart.Line("t", "y", []chart.Series{
		{Name: "nan", Points: [][2]float64{{0, math.NaN()}, {1, 2}, {math.Inf(1), 3}, {2, math.Inf(-1)}}},
		{Name: "empty"},
		{Name: "one", Points: [][2]float64{{5, 5}}},
	}, []chart.Marker{{X: 1, Label: long}}, []chart.Band{{X0: 0, X1: 1, Label: "p"}}, chart.Axis{})
	for _, bad := range []string{"NaN", "Inf", "+Inf", "-Inf"} {
		if strings.Contains(svg, bad) {
			t.Fatalf("chart contains %s", bad)
		}
	}
	// the full label stays in the marker's tooltip; the drawn text is cut
	if strings.Count(svg, long) != 1 || !strings.Contains(svg, `class="marklabel"`) || !strings.Contains(svg, "…</text>") {
		t.Fatal("the long marker label was not truncated in the drawn text")
	}
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "<path") {
		t.Fatal("no chart drawn")
	}
}

// A sample with NaN or Inf in a vital is still written to the results stream as a valid
// line, with the bad fields zeroed: one unreadable node never loses a record.
func TestEmit_MarshalFailureIsRecorded(t *testing.T) {
	var out bytes.Buffer
	r := &Run{Out: &out}
	r.emit(Record{Kind: "sample", S: &Sample{CPUPct: math.NaN(), MachinePct: math.Inf(1), Load1: 0.5, DiskWriteBps: math.Inf(-1)}})
	line := bytes.TrimSpace(out.Bytes())
	if !json.Valid(line) {
		t.Fatalf("results line is not JSON: %s", line)
	}
	var back Record
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if back.S == nil || back.S.CPUPct != 0 || back.S.MachinePct != 0 || back.S.DiskWriteBps != 0 || back.S.Load1 != 0.5 {
		t.Fatalf("sample after scrub: %+v", back.S)
	}
	if len(r.records) != 1 || r.records[0].Line != 1 {
		t.Fatalf("records: %+v", r.records)
	}
}

// A node's cumulative counters reset when it restarts: the rate for that interval is 0,
// never negative or enormous, and the next interval is computed from the new base.
// Invariant: I-190
func TestVitals_CounterReset(t *testing.T) {
	r := &Run{}
	now := r.now()
	r.vitals = map[string]vitalsPrev{"n1": {t: now.Add(-time.Second), m: admin.MachineReport{CPUSeconds: 100, MachineBusy: 900, MachineTotal: 1000, DiskWritten: 1 << 30, DiskRead: 1 << 20, NetRx: 1 << 40, NetTx: 1 << 40, TailnetRx: 1 << 30, TailnetTx: 1 << 30}}}
	var s Sample
	r.vitalsSample(&s, "n1", admin.MachineReport{CPUSeconds: 0.5, MachineBusy: 10, MachineTotal: 20, DiskWritten: 4096, DiskRead: 0, NetRx: 10, NetTx: 10, TailnetRx: 1, TailnetTx: 1})
	if s.CPUPct != 0 || s.MachinePct != 0 || s.DiskWriteBps != 0 || s.DiskReadBps != 0 || s.NetRxBps != 0 || s.TailnetTxBps != 0 {
		t.Fatalf("rates across a reset: %+v", s)
	}
	for _, v := range []float64{s.CPUPct, s.MachinePct, s.DiskWriteBps, s.DiskReadBps, s.NetRxBps, s.NetTxBps, s.TailnetRxBps, s.TailnetTxBps} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			t.Fatalf("bad rate %v", v)
		}
	}
	// the base moved to the post-reset report
	prev := r.vitals["n1"]
	if prev.m.CPUSeconds != 0.5 {
		t.Fatalf("base not reset: %+v", prev.m)
	}
	// too soon after: the last rates are carried, not recomputed over milliseconds
	r.vitals["n1"] = vitalsPrev{t: now, m: prev.m, cpu: 7, trx: 9, hasRates: true}
	var s2 Sample
	r.vitalsSample(&s2, "n1", admin.MachineReport{CPUSeconds: 100})
	if s2.CPUPct != 7 || s2.TailnetRxBps != 9 {
		t.Fatalf("carried rates %v %v, want 7 and 9", s2.CPUPct, s2.TailnetRxBps)
	}
	// a plain interval: bytes per second from the counters' difference
	r.vitals["n1"] = vitalsPrev{t: now.Add(-2 * time.Second), m: admin.MachineReport{NetRx: 1000, NetTx: 0, TailnetRx: 500, TailnetTx: 100}}
	var s3 Sample
	r.vitalsSample(&s3, "n1", admin.MachineReport{NetRx: 3000, NetTx: 4000, TailnetRx: 700, TailnetTx: 100})
	near := func(got, want float64) bool { return math.Abs(got-want) <= want*0.01 } // the run's clock is a hair past the fixture's two seconds
	if !near(s3.NetRxBps, 1000) || !near(s3.NetTxBps, 2000) || !near(s3.TailnetRxBps, 100) || s3.TailnetTxBps != 0 {
		t.Fatalf("network rates %v %v %v %v", s3.NetRxBps, s3.NetTxBps, s3.TailnetRxBps, s3.TailnetTxBps)
	}
}

// The slot table of a malformed chunk report (a hash shorter than the prefix it prints)
// is a message, not a panic.
func TestSlotTable_ShortHash(t *testing.T) {
	if got := slotTable(admin.ChunkReport{Hash: "abc"}); !strings.Contains(got, "malformed") {
		t.Fatalf("short hash: %q", got)
	}
	if got := slotTable(admin.ChunkReport{Hash: strings.Repeat("a", 64), K: 2, M: 1}); !strings.HasPrefix(got, "aaaaaaaaaaaa 2+1") {
		t.Fatalf("well-formed: %q", got)
	}
}
