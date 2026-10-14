package stats

import (
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chart"
)

// A series is folded to at most chart.Points buckets by the mean, deterministically, and
// fewer points pass through untouched: the boundary table on the count.
// Invariant: I-196
func TestStatsQuery_Downsample_Points_Table(t *testing.T) {
	for _, n := range []int{0, 1, 2, chart.Points - 1, chart.Points, chart.Points + 1, 10 * chart.Points} {
		pts := make([][2]float64, n)
		for i := range pts {
			pts[i] = [2]float64{float64(i), float64(i % 5)}
		}
		got := Downsample(pts, chart.Points)
		if n <= chart.Points {
			if len(got) != n {
				t.Errorf("%d points: %d after downsampling, want unchanged", n, len(got))
			}
			continue
		}
		if len(got) > chart.Points || len(got) < chart.Points/2 {
			t.Errorf("%d points: %d buckets", n, len(got))
		}
		again := Downsample(pts, chart.Points)
		for i := range got {
			if got[i] != again[i] {
				t.Fatalf("%d points: not deterministic at %d", n, i)
			}
			if got[i][1] < 0 || got[i][1] > 4 {
				t.Fatalf("%d points: a mean outside the data: %v", n, got[i])
			}
		}
		if i := len(got) / 2; i > 0 && got[i][0] <= got[i-1][0] {
			t.Errorf("%d points: x not increasing", n)
		}
	}
	if got := Downsample([][2]float64{{3, 1}, {3, 3}}, 1); len(got) != 1 || got[0][1] != 2 {
		t.Errorf("one bucket: %v", got)
	}
}

// The page's charts: the cluster charts from the cluster rows with markers for a leader
// change, a death, a return and a repair pass; the node charts for one node, or one series
// per node for "all"; no chart for a metric nobody reported.
// Invariant: I-196
func TestStatsQuery_Charts(t *testing.T) {
	from := utc(2026, 10, 11, 8, 0)
	to := from.Add(6 * time.Hour)
	var rows []Row
	for s := 0; s < 6*3600; s += 60 {
		at := from.Add(time.Duration(s) * time.Second)
		live := map[string]int{"a": 1, "b": 1}
		if s >= 3600 && s < 5400 {
			live["b"] = 0
		}
		moved := 0.0
		if s >= 7200 && s < 9000 {
			moved = 1 << 20
		}
		term := uint64(3)
		if s >= 10800 {
			term = 4
		}
		rows = append(rows,
			Row{T: at.Unix(), Term: term, By: "a", C: &Cluster{
				Chunks: 100, Stored: 1e9, Logical: 7e8, Live: live, RepairMoved: moved,
				Targets: []chart.Target{{Name: "a/1", Host: "a", Slots: 50, Capacity: 1e9, Live: true, Active: true}, {Name: "b/1", Host: "b", Slots: 50, Capacity: 1e9, Live: live["b"] == 1, Active: true}},
			}},
			Row{T: at.Unix(), N: "a", M: map[string]float64{"cpu": 0.1, "bytes_w": 1 << 20, "rss": 1e8}},
			Row{T: at.Unix(), N: "b", M: map[string]float64{"cpu": 0.2, "bytes_w": 2 << 20}})
	}
	charts := Charts(rows, from, to, "a")
	ids := map[string]Chart{}
	for _, c := range charts {
		ids[c.ID] = c
	}
	for _, want := range []string{"stored", "chunks", "backlog", "moved", "liveness", "spread", "slots", "throughput", "cpu", "memory"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("chart %s missing", want)
		}
	}
	for _, absent := range []string{"tailnet", "net", "pressure", "diskfree", "diskio", "wal", "process", "readahead"} {
		if _, ok := ids[absent]; ok {
			t.Errorf("chart %s drawn without a metric", absent)
		}
	}
	for _, m := range []string{"b dead", "b live", "repair pass start", "repair pass end", "leader a (term 4)"} {
		if !strings.Contains(ids["stored"].SVG, m) {
			t.Errorf("marker %q missing", m)
		}
	}
	if !strings.Contains(ids["cpu"].SVG, ">08:00</text>") || !strings.Contains(ids["cpu"].SVG, ">14:00</text>") {
		t.Error("the clock axis is missing")
	}
	if !strings.Contains(ids["throughput"].SVG, "write MiB/s") || strings.Contains(ids["throughput"].SVG, ">b</text>") {
		t.Error("one node: the series are its metrics")
	}
	all := Charts(rows, from, to, "all")
	for _, c := range all {
		if c.ID == "throughput" && (!strings.Contains(c.SVG, ">a</text>") || !strings.Contains(c.SVG, ">b</text>")) {
			t.Error("all nodes: one series per node")
		}
	}
	if len(Charts(nil, from, to, "a")) != 0 {
		t.Error("no rows: charts drawn")
	}
}

// The node section draws the paths lanes and the round-trip line from exactly the
// path.<peer> and rtt.<peer> keys: one lane and one series per peer with a key for one
// node, one lane per ordered pair and one series per member (its mean) with "all",
// never a lane for a peer without a key, and nothing on a device without a mesh; the
// mesh network chart carries the relayed series.
// Invariant: I-332
func TestStatsQuery_MeshCharts(t *testing.T) {
	from := utc(2026, 10, 11, 8, 0)
	to := from.Add(time.Hour)
	var rows []Row
	for s := 0; s < 3600; s += 60 {
		at := from.Add(time.Duration(s) * time.Second).Unix()
		pathB := 3.0
		if s >= 1800 {
			pathB = 0
		}
		rows = append(rows,
			Row{T: at, N: "a", M: map[string]float64{"path.b": pathB, "rtt.b": 4, "path.c": 1, "rtt.c": 60, "mesh_rx": 1, "relayed": 2 << 20}},
			Row{T: at, N: "b", M: map[string]float64{"path.a": 3, "rtt.a": 4}},
			Row{T: at, N: "c", M: map[string]float64{"cpu": 0.1}}) // no mesh
	}
	byID := func(node string) map[string]Chart {
		out := map[string]Chart{}
		for _, c := range Charts(rows, from, to, node) {
			out[c.ID] = c
		}
		return out
	}
	a := byID("a")
	if !strings.Contains(a["paths"].SVG, ">b</text>") || !strings.Contains(a["paths"].SVG, ">c</text>") || strings.Contains(a["paths"].SVG, ">a</text>") {
		t.Errorf("a's lanes: %s", a["paths"].Title)
	}
	if n := strings.Count(a["paths"].SVG, `class="chart-bad"`); n == 0 || strings.Count(a["paths"].SVG, `class="chart-ok"`) == 0 || strings.Count(a["paths"].SVG, `class="chart-off"`) == 0 {
		t.Errorf("a's lanes miss a class: bad %d", n)
	}
	if !strings.Contains(a["rtt"].SVG, ">b</text>") || !strings.Contains(a["rtt"].SVG, ">c</text>") || a["rtt"].Title != "Mesh round trip from a" {
		t.Errorf("a's round trips: %q", a["rtt"].Title)
	}
	if !strings.Contains(a["tailnet"].SVG, "relayed MiB/s") {
		t.Error("the mesh network chart lacks the relayed series")
	}
	c := byID("c")
	if _, ok := c["paths"]; ok {
		t.Error("a device without a mesh drew paths")
	}
	if _, ok := c["rtt"]; ok {
		t.Error("a device without a mesh drew round trips")
	}
	all := byID("all")
	for _, lane := range []string{"a → b", "a → c", "b → a"} {
		if !strings.Contains(all["paths"].SVG, ">"+lane+"</text>") {
			t.Errorf("every pair: lane %q missing", lane)
		}
	}
	if strings.Contains(all["paths"].SVG, ">c → ") {
		t.Error("a lane for a device without keys")
	}
	if !strings.Contains(all["rtt"].SVG, ">a</text>") || !strings.Contains(all["rtt"].SVG, ">b</text>") || strings.Contains(all["rtt"].SVG, ">c</text>") {
		t.Error("every member: one series per member with round trips")
	}
	if all["paths"].Title != "Mesh paths (every pair)" || all["rtt"].Title != "Mesh round trip (mean per member)" {
		t.Errorf("titles %q %q", all["paths"].Title, all["rtt"].Title)
	}
}
