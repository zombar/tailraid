package chart

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The golden data set: rendered once by the soak report's helpers before their move here
// (colours mapped to the classes this package emits) and kept in testdata/, so the move
// changed nothing about the drawing. The literals below are the same data set.

const longLabel = "a very long label that goes past forty characters for sure"

func goldenLine() string {
	return Line("Throughput (phase bands, event markers)", "MiB/s", []Series{
		{Name: "write MiB/s", Points: [][2]float64{{0, 0}, {1, 2.5}, {2, 1}, {3, math.NaN()}, {4, 3}}},
		{Name: "read MiB/s", Points: [][2]float64{{0, 1}, {2, 0.5}}},
		{Name: "one <b>", Points: [][2]float64{{1, 1}}},
	}, []Marker{{1.5, "kill n1"}, {2.5, longLabel}}, []Band{{0, 2, "1 baseline"}, {2, 4, "2 kill"}}, Axis{})
}

func goldenBars() string {
	return Bars("Large file: shard slots per node (x)", []string{"a", "b"}, []Series{
		{Name: "data slots", Points: [][2]float64{{0, 3}, {0, 1}}},
		{Name: "parity slots", Points: [][2]float64{{0, 1}, {0, 2}}},
		{Name: "capacity share", Class: "chart-off", Points: [][2]float64{{0, 2}, {0, 2}}},
	})
}

func goldenLanes() string {
	return Lanes("Node liveness", 10, []string{"a", "b"}, map[string][]Cell{
		"a": {{0, OK}, {5, Bad}, {10, OK}},
		"b": {{0, OK}, {5, Warn}},
	}, Axis{})
}

func goldenStrip() string {
	return Strip("FSM-hash agreement", 10, []Cell{{0, OK}, {5, Bad}, {10, OK}}, Axis{})
}

func goldenTargets() []Target {
	return []Target{
		{Name: "a", Host: "a", Slots: 10, Capacity: 1, Live: true, Active: true},
		{Name: "a/1", Host: "a", Slots: 10, Capacity: 1, Live: false, Active: true},
		{Name: "b", Host: "b", Slots: 0, Capacity: 1, Live: true, Active: false},
		{Name: "c", Host: "c", Slots: 14, Capacity: 1, Live: true, Active: true},
	}
}

func goldenSpread() string {
	ts := goldenTargets()
	return Spread(10, []float64{0, 5}, []SpreadSample{{Targets: ts, Chunks: 10, DomainSkew: 3}, {Targets: ts, Chunks: 10}}, Axis{})
}

// everyKind renders one chart of each kind.
func everyKind() map[string]string {
	return map[string]string{"line": goldenLine(), "bars": goldenBars(), "lanes": goldenLanes(), "strip": goldenStrip(), "spread": goldenSpread()}
}

// Invariant: I-197
func TestChart_ReportGolden_ByteIdenticalAfterMove(t *testing.T) {
	for name, got := range everyKind() {
		want, err := os.ReadFile("testdata/" + name + ".svg")
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s: differs from the report's rendering before the move\n got: %s\nwant: %s", name, got, want)
		}
	}
}

var (
	classAttr = regexp.MustCompile(`class="([^"]+)"`)
	colourRe  = regexp.MustCompile(`(?:fill|stroke)="#`)
)

// Invariant: I-197
func TestChart_NoInlineStyle_NoColour(t *testing.T) {
	known := map[string]bool{}
	for _, c := range Classes {
		known[c] = true
	}
	for name, svg := range everyKind() {
		if strings.Contains(svg, "style=") {
			t.Errorf("%s: emits an inline style", name)
		}
		if colourRe.MatchString(svg) {
			t.Errorf("%s: emits a colour", name)
		}
		for _, m := range classAttr.FindAllStringSubmatch(svg, -1) {
			for c := range strings.FieldsSeq(m[1]) {
				if !known[c] {
					t.Errorf("%s: emits class %q, not in Classes", name, c)
				}
			}
		}
	}
}

// Invariant: I-197
func TestChart_ClassesDefinedInAppCSS(t *testing.T) {
	css, err := os.ReadFile("../webui/src/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range Classes {
		if !strings.Contains(string(css), "."+c+" ") && !strings.Contains(string(css), "."+c+",") && !strings.Contains(string(css), "."+c+" {") {
			t.Errorf("app.css does not define .%s", c)
		}
	}
}

// Invariant: I-197
func TestChart_EscapesHostileNames(t *testing.T) {
	hostile := `<script>alert(1)</script>&"'`
	ss := []Series{{Name: hostile, Points: [][2]float64{{0, 1}, {1, 2}}}}
	for name, svg := range map[string]string{
		"line":   Line(hostile, hostile, ss, []Marker{{0.5, hostile}}, []Band{{0, 1, hostile}}, Axis{}),
		"bars":   Bars(hostile, []string{hostile}, ss),
		"lanes":  Lanes(hostile, 1, []string{hostile}, map[string][]Cell{hostile: {{0, OK}}}, Axis{}),
		"strip":  Strip(hostile, 1, []Cell{{0, OK}}, Axis{}),
		"spread": Spread(1, []float64{0}, []SpreadSample{{Targets: []Target{{Name: hostile, Host: hostile, Slots: 1, Capacity: 1, Live: true, Active: true}}, Chunks: 1}}, Axis{}),
	} {
		if strings.Contains(svg, "<script") || strings.Contains(svg, `alert(1)</`) {
			t.Errorf("%s: hostile text not escaped", name)
		}
		if !strings.Contains(svg, "&lt;script&gt;") {
			t.Errorf("%s: escaped form missing", name)
		}
	}
}

// Invariant: I-197
func TestChart_Points_Table(t *testing.T) {
	pathRe := regexp.MustCompile(`<path d="([^"]*)"`)
	for _, n := range []int{0, 1, 2, Points - 1, Points, Points + 1} {
		pts := make([][2]float64, n)
		for i := range pts {
			pts[i] = [2]float64{float64(i), float64(i % 7)}
		}
		svg := Line("t", "y", []Series{{Name: "s", Points: pts}}, nil, nil, Axis{})
		m := pathRe.FindStringSubmatch(svg)
		if m == nil {
			t.Fatalf("%d points: no path", n)
		}
		cmds := strings.Count(m[1], "L")
		if n == 0 && m[1] != "" {
			t.Errorf("0 points: path %q, want empty", m[1])
		}
		if n >= 1 && cmds != n-1 {
			t.Errorf("%d points: %d line commands, want %d", n, cmds, n-1)
		}
		if circles := strings.Count(svg, "<circle"); (n == 1) != (circles == 1) {
			t.Errorf("%d points: %d circles", n, circles)
		}
		if !strings.Contains(svg, `class="tick">s</text>`) {
			t.Errorf("%d points: the legend entry is missing", n)
		}
	}
}

// Invariant: I-197
func TestChart_LegendWrap_Table(t *testing.T) {
	heightRe := regexp.MustCompile(`viewBox="0 0 760 (\d+)"`)
	// an entry of "series-xxxx" is 105.5 units wide, so six fit a 688-unit row
	for _, tc := range []struct{ n, rows int }{{0, 0}, {1, 1}, {6, 1}, {7, 2}, {20, 4}} {
		ss := make([]Series, tc.n)
		for i := range ss {
			ss[i] = Series{Name: "series-" + strings.Repeat("x", 4), Points: [][2]float64{{0, 1}}}
		}
		svg := Line("t", "y", ss, nil, nil, Axis{})
		m := heightRe.FindStringSubmatch(svg)
		if m == nil {
			t.Fatalf("%d series: no viewBox", tc.n)
		}
		want := Height
		if tc.rows > 0 {
			want += 6 + tc.rows*legendRow
		}
		if m[1] != strconv.Itoa(want) {
			t.Errorf("%d series: height %s, want %d (%d legend rows)", tc.n, m[1], want, tc.rows)
		}
		if got := strings.Count(svg, `class="tick">series-`); got != tc.n {
			t.Errorf("%d series: %d legend entries", tc.n, got)
		}
	}
}

// Invariant: I-197
func TestChart_DropsNaNInf(t *testing.T) {
	svg := Line("t", "y", []Series{{Name: "s", Points: [][2]float64{{0, 1}, {1, math.NaN()}, {2, math.Inf(1)}, {math.Inf(-1), 2}, {3, 4}}}}, nil, nil, Axis{})
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("NaN or Inf in the output: %s", svg)
	}
	if got := strings.Count(regexp.MustCompile(`<path d="([^"]*)"`).FindStringSubmatch(svg)[1], "L"); got != 1 {
		t.Errorf("%d line commands after dropping, want 1", got)
	}
}

// Invariant: I-197
func TestChart_Spread_DeadAndDraining(t *testing.T) {
	svg := goldenSpread()
	for _, want := range []string{`fill="url(#dead)"`, `>a/1 at 0s: dead</title>`, `>b at 0s: draining</title>`, `class="dev0"`, `class="chart-warn"><title>0s: 3 chunks above the host cap`, `class="chart-ok"><title>5s: no chunk above the host cap`} {
		if !strings.Contains(svg, want) {
			t.Errorf("missing %q", want)
		}
	}
	if Spread(10, []float64{0}, []SpreadSample{{}}, Axis{}) != "" {
		t.Error("a sample without targets should draw nothing")
	}
}

// Invariant: I-197
func TestChart_ClockAxis(t *testing.T) {
	start := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	ss := []Series{{Name: "v", Points: [][2]float64{{0, 1}, {21600, 2}}}}
	day := Line("t", "y", ss, nil, nil, Axis{Clock: true, Start: start})
	for _, want := range []string{`>08:00</text>`, `>11:00</text>`, `>14:00</text>`} {
		if !strings.Contains(day, want) {
			t.Errorf("6 h: missing tick %q", want)
		}
	}
	if regexp.MustCompile(`\ds</text>`).MatchString(day) {
		t.Error("6 h: a seconds tick remains")
	}
	three := Line("t", "y", []Series{{Name: "v", Points: [][2]float64{{0, 1}, {3 * 86400, 2}}}}, nil, nil, Axis{Clock: true, Start: start})
	for _, want := range []string{`>10-11 08:00</text>`, `>10-14 08:00</text>`} {
		if !strings.Contains(three, want) {
			t.Errorf("3 d: missing tick %q", want)
		}
	}
	sp := Spread(21600, []float64{0, 1200}, []SpreadSample{{Targets: goldenTargets(), Chunks: 10}, {Targets: goldenTargets(), Chunks: 10}}, Axis{Clock: true, Start: start})
	if !strings.Contains(sp, ` at 08:20: `) {
		t.Error("spread: the tooltip does not print the clock")
	}
	ln := Lanes("l", 21600, []string{"a"}, map[string][]Cell{"a": {{0, OK}}}, Axis{Clock: true, Start: start})
	if !strings.Contains(ln, `>14:00</text>`) {
		t.Error("lanes: the clock axis is missing")
	}
}

// Invariant: I-197
func TestExpectedShares_WaterFill(t *testing.T) {
	ts := []Target{{Capacity: 1, Active: true, Slots: 30}, {Capacity: 3, Active: true, Slots: 30}, {Capacity: 1, Active: false, Slots: 0}}
	// 60 slots over capacities 1 and 3 would be 15 and 45, but no target may expect more
	// than 20 (one slot per chunk, I-9): the second is capped, the rest flows to the first,
	// which is capped in turn; the draining target expects nothing
	if got := ExpectedShares(ts, 20); got[0] != 20 || got[1] != 20 || got[2] != 0 {
		t.Errorf("capped: got %v, want [20 20 0]", got)
	}
	if got := ExpectedShares(ts, 1000); got[0] != 15 || got[1] != 45 || got[2] != 0 {
		t.Errorf("uncapped: got %v, want [15 45 0]", got)
	}
}

// Invariant: I-197
func TestChart_TruncateMarkerLabel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"abc", "abc"}, {"abcd", "ab…"}, {"", ""}} {
		if got := truncate(tc.in, 3); got != tc.want {
			t.Errorf("truncate(%q, 3) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Invariant: I-197
func TestChart_AxisSpan(t *testing.T) {
	ss := []Series{{Name: "v", Points: [][2]float64{{0, 1}, {100, 2}}}}
	free := Line("t", "y", ss, nil, nil, Axis{})
	if !strings.Contains(free, `>100s</text>`) {
		t.Error("without a span the axis ends at the data")
	}
	pinned := Line("t", "y", ss, nil, nil, Axis{Span: 600})
	if !strings.Contains(pinned, `>600s</text>`) || !strings.Contains(pinned, `>500s</text>`) {
		t.Error("with a span the axis ends at the span")
	}
}
