package soak

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chart"
)

// Node names, check text and file names from the environment are untrusted: the report
// escapes them everywhere they appear, including inside the SVG charts.
func TestRender_EscapesHostileNodeNamesAndErrors(t *testing.T) {
	hostile := `<script>alert(1)</script>`
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s := Summary{
		Env: "env " + hostile, Started: start, Duration: 10 * time.Second, Seed: 7, Versions: map[string]string{hostile: "v" + hostile},
		Nodes:  []NodeRef{{Name: hostile, Idx: 1, RPC: "1.2.3.4:1"}},
		Phases: []PhaseResult{{Name: "1 x", Failed: 1, Checks: []Record{{Kind: "check", Name: "chk " + hostile, Cites: "I-1", OK: new(false), Text: "err " + hostile, File: "/f/" + hostile, Node: hostile, Line: 3}}}},
		Records: []Record{
			{T: start, Kind: "phase", Name: "1 x", Text: "start"},
			{T: start.Add(time.Second), Kind: "sample", S: &Sample{Slots: map[string]int{hostile: 3}, LiveNodes: map[string]bool{hostile: true}, FSMAgree: new(true)}},
			{T: start.Add(2 * time.Second), Kind: "event", Name: "kill", Text: hostile},
			{T: start.Add(3 * time.Second), Kind: "event", Name: "note", Text: "note " + hostile},
			{T: start.Add(4 * time.Second), Kind: "sample", S: &Sample{BytesWritten: 1 << 20, Slots: map[string]int{hostile: 2}, LiveNodes: map[string]bool{hostile: false}}},
			{T: start.Add(5 * time.Second), Kind: "dump", Phase: "1 x", Node: hostile, Text: "goroutine 1 [select]:\nmain.main()\n" + hostile + "\n"},
		},
		Latency:  []LatencyRow{{Phase: "1 x", Kind: "write", N: 1, P50: time.Millisecond, P99: time.Millisecond, Max: time.Millisecond}},
		Coverage: []CoverageRow{{ID: "I-1", Failed: 1}},
	}
	var out bytes.Buffer
	if err := Render(&out, s); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, hostile) {
		i := strings.Index(html, hostile)
		t.Fatalf("raw hostile string in report near: %q", html[max(0, i-80):i+40])
	}
	for _, want := range []string{"&lt;script&gt;", "FAIL", "results.jsonl:3", "seed <code>7</code>", "I-1 0/<b>1 failed</b>", "note &lt;script&gt;", "Node liveness", "Goroutine dumps", "(3 lines)", "goroutine 1 [select]:"} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Count(html, "<svg") < 7 {
		t.Errorf("only %d charts", strings.Count(html, "<svg"))
	}
}

// A run that skipped a phase for a missing capability is PARTIAL, never PASS; a failed
// check is FAIL even when phases were skipped.
func TestReport_PartialWhenPhasesSkipped(t *testing.T) {
	cases := []struct {
		failed  int
		partial bool
		want    string
	}{{0, false, "PASS"}, {0, true, "PARTIAL"}, {1, true, "FAIL"}, {1, false, "FAIL"}}
	for _, c := range cases {
		d := buildReport(Summary{Failed: c.failed, Partial: c.partial, Phases: []PhaseResult{{Name: "4 parity", Skipped: c.partial, Reason: "capability"}}})
		if d.Verdict != c.want {
			t.Errorf("failed=%d partial=%v: verdict %s, want %s", c.failed, c.partial, d.Verdict, c.want)
		}
	}
}

// Percentiles are nearest-rank; fault windows span kill/partition to start/heal; latency
// rows separate the whole phase from the operations issued under a fault; coverage counts
// pass and fail per cited ID in numeric order, invariants first.
func TestSummary_LatencyWindowsAndCoverage(t *testing.T) {
	var ds []time.Duration
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	if p50, p99, mx := percentiles(ds); p50 != 50*time.Millisecond || p99 != 99*time.Millisecond || mx != 100*time.Millisecond {
		t.Fatalf("percentiles %v %v %v", p50, p99, mx)
	}
	if p50, p99, mx := percentiles(nil); p50 != 0 || p99 != 0 || mx != 0 {
		t.Fatal("empty percentiles")
	}
	if p50, _, mx := percentiles([]time.Duration{7}); p50 != 7 || mx != 7 {
		t.Fatal("single sample")
	}
	t0 := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	recs := []Record{
		{T: at(1), Kind: "event", Name: "kill", Text: "n3"},
		{T: at(2), Kind: "event", Name: "kill", Text: "n2"}, // still the same window
		{T: at(5), Kind: "event", Name: "start", Text: "n3"},
		{T: at(8), Kind: "event", Name: "partition", Text: "a|b"},
		{T: at(9), Kind: "event", Name: "note", Text: "x"},
		{T: at(12), Kind: "event", Name: "heal"},
		{T: at(20), Kind: "event", Name: "restart all"}, // never closed: open-ended
	}
	ws := faultWindows(recs)
	if len(ws) != 3 || !ws[0].from.Equal(at(1)) || !ws[0].to.Equal(at(5)) || !ws[1].from.Equal(at(8)) || !ws[1].to.Equal(at(12)) || !ws[2].to.IsZero() {
		t.Fatalf("windows %+v", ws)
	}
	for _, c := range []struct {
		s    int
		want bool
	}{{0, false}, {1, true}, {4, true}, {5, false}, {10, true}, {15, false}, {25, true}} {
		if got := inWindows(ws, at(c.s)); got != c.want {
			t.Errorf("t=%ds in windows = %v", c.s, got)
		}
	}
	lats := []opLatency{
		{T: at(0), Phase: "4 parity", D: 2 * time.Millisecond},
		{T: at(3), Phase: "4 parity", D: 20 * time.Millisecond},
		{T: at(4), Phase: "4 parity", Read: true, D: 5 * time.Millisecond},
		{T: at(6), Phase: "4 parity", D: 1 * time.Millisecond},
		{T: at(7), Phase: "5 corruption", Read: true, D: 3 * time.Millisecond},
	}
	rows := latencyRows(lats, []PhaseResult{{Name: "4 parity"}, {Name: "5 corruption"}}, ws)
	want := []LatencyRow{
		{Phase: "4 parity", Kind: "write", N: 3, P50: 2 * time.Millisecond, P99: 20 * time.Millisecond, Max: 20 * time.Millisecond},
		{Phase: "4 parity", Kind: "read", N: 1, P50: 5 * time.Millisecond, P99: 5 * time.Millisecond, Max: 5 * time.Millisecond},
		{Phase: "4 parity", Kind: "write", UnderFaults: true, N: 1, P50: 20 * time.Millisecond, P99: 20 * time.Millisecond, Max: 20 * time.Millisecond},
		{Phase: "4 parity", Kind: "read", UnderFaults: true, N: 1, P50: 5 * time.Millisecond, P99: 5 * time.Millisecond, Max: 5 * time.Millisecond},
		{Phase: "5 corruption", Kind: "read", N: 1, P50: 3 * time.Millisecond, P99: 3 * time.Millisecond, Max: 3 * time.Millisecond},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, rows[i], want[i])
		}
	}
	cov := coverage([]PhaseResult{{Checks: []Record{
		{Cites: "L-10 I-12", OK: new(true)}, {Cites: "I-2 L-10", OK: new(false)}, {Cites: "I-12"},
	}}})
	wantCov := []CoverageRow{{ID: "I-2", Failed: 1}, {ID: "I-12", Passed: 2}, {ID: "L-10", Passed: 1, Failed: 1}}
	if len(cov) != len(wantCov) {
		t.Fatalf("coverage %+v", cov)
	}
	for i := range wantCov {
		if cov[i] != wantCov[i] {
			t.Errorf("coverage %d: %+v, want %+v", i, cov[i], wantCov[i])
		}
	}
}

// Every invariant and lifecycle cell a phase cites is defined in the design docs, so the
// report's coverage table never names an ID that does not exist.
func TestPhases_CitesAreDefined(t *testing.T) {
	var src []byte
	for _, f := range []string{"phases.go", "automation.go", "largefile.go", "disks.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src = append(src, b...)
	}
	defined := map[string]bool{}
	for _, doc := range []string{"../../docs/invariants.md", "../../docs/lifecycle.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`\| ([IL]-\d+) \|`).FindAllStringSubmatch(string(b), -1) {
			defined[m[1]] = true
		}
	}
	if len(defined) < 100 {
		t.Fatalf("only %d IDs found in the generated docs", len(defined))
	}
	cites := regexp.MustCompile(`r\.check(?:At|Skipped|FSMAgreement\(ctx, |\()"([^"]*)"`)
	n := 0
	for _, m := range cites.FindAllStringSubmatch(string(src), -1) {
		for id := range strings.FieldsSeq(m[1]) {
			n++
			if !defined[id] {
				t.Errorf("a phase cites %s, which no design doc defines", id)
			}
		}
	}
	if n < 50 {
		t.Fatalf("only %d citations found", n)
	}
}

// The report's stylesheet defines every class the chart package emits, in the report's
// own colours, as app.css does in the web UI's tokens.
// Invariant: I-197
func TestReport_DefinesChartClasses(t *testing.T) {
	var out bytes.Buffer
	if err := Render(&out, Summary{Started: time.Now(), Phases: nil}); err != nil {
		t.Fatal(err)
	}
	style := out.String()
	for _, c := range chart.Classes {
		if !strings.Contains(style, "."+c+"{") && !strings.Contains(style, "."+c+",") {
			t.Errorf("the report's stylesheet does not define .%s", c)
		}
	}
}

// The report draws the network chart when any sample carries a network rate (I-190), and
// leaves it out when none does, like the other vitals charts.
// Invariant: I-190
func TestReport_NetworkChart(t *testing.T) {
	render := func(s Sample) string {
		var out bytes.Buffer
		now := time.Now()
		sum := Summary{Started: now, Duration: 2 * time.Second, Records: []Record{{Kind: "sample", T: now.Add(time.Second), S: &s}}}
		if err := Render(&out, sum); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	const title = "Network on the sampled node (tailnet and every interface)"
	with := render(Sample{MeshRxBps: 3 << 20, NetRxBps: 4 << 20})
	if !strings.Contains(with, title) || !strings.Contains(with, "mesh rx MiB/s") || !strings.Contains(with, "rx MiB/s (every interface)") {
		t.Fatal("the network chart is missing with a rate sampled")
	}
	if strings.Contains(render(Sample{Load1: 1}), title) {
		t.Fatal("the network chart is drawn without a network rate")
	}
}
