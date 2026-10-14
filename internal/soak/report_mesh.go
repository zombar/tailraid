package soak

import (
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/chart"
)

// The mesh figures of phases 25 and 26 (docs/20 §10, docs/09 §6, the lookdev
// assets/lookdev/soak-mesh.html): a lane per ordered pair with its path class over the
// run, each device's mesh traffic, the paths table and the join round-trip table. Drawn
// only on a run whose samples carry paths.

// pairView is one row of the paths table.
type pairView struct {
	From, To, Class, Endpoint, Handshake string
}

// joinView is the round-trip table of the late device.
type joinView struct {
	Late, PendingOn, Pending, Join, Handshake, Log string
}

// pathStatus is the lane cell class of a path class.
func pathStatus(class string) chart.Status {
	switch class {
	case "lan":
		return chart.OK
	case "wan":
		return chart.Warn
	case "relay":
		return chart.Off
	}
	return chart.Bad
}

// meshFigures draws the lanes and the traffic chart and builds the tables from the records.
func meshFigures(s Summary) (charts []template.HTML, pairs []pairView, join *joinView) {
	x := func(rec *Record) float64 { return rec.T.Sub(s.Started).Seconds() }
	cells := map[string][]chart.Cell{}
	var names []string
	traffic := map[string]*chart.Series{}
	prev := map[string][3]float64{} // node → x, rx, tx of its previous sample
	var pathsRec *Record
	for i := range s.Records {
		rec := &s.Records[i]
		switch {
		case rec.Kind == "sample" && rec.S != nil && len(rec.S.Paths) > 0:
			for pair, class := range rec.S.Paths {
				if _, ok := cells[pair]; !ok {
					names = append(names, pair)
				}
				cells[pair] = append(cells[pair], chart.Cell{X: x(rec), Status: pathStatus(class)})
			}
			for node, b := range rec.S.MeshBytes {
				cur := [3]float64{x(rec), float64(b[0]), float64(b[1])}
				if p, ok := prev[node]; ok && cur[0] > p[0] && cur[1] >= p[1] && cur[2] >= p[2] {
					sr := traffic[node]
					if sr == nil {
						sr = &chart.Series{Name: node}
						traffic[node] = sr
					}
					sr.Points = append(sr.Points, [2]float64{cur[0], (cur[1] - p[1] + cur[2] - p[2]) / (cur[0] - p[0]) / (1 << 20)})
				}
				prev[node] = cur
			}
		case rec.Kind == "event" && rec.Name == meshPathsEvent:
			pathsRec = rec // the phase records one per run
		case rec.Kind == "event" && rec.Name == meshJoinEvent:
			a := rec.Attrs
			join = &joinView{Late: a["late"], PendingOn: a["pending_on"], Pending: ms(a["pending_ms"]), Join: ms(a["join_ms"]), Handshake: ms(a["handshake_ms"]), Log: ms(a["log_ms"])}
		}
	}
	if len(names) == 0 && pathsRec == nil && join == nil {
		return nil, nil, nil
	}
	sort.Strings(names)
	maxX := s.Duration.Seconds()
	if len(names) > 0 {
		charts = append(charts, template.HTML(chart.Lanes("Path class of every pair as each node sees it (green lan, amber wan, grey relay, red none)", maxX, names, cells, chart.Axis{}))) //nolint:gosec // generated SVG
	}
	if len(traffic) > 0 {
		var series []chart.Series
		for _, node := range sortedKeys(traffic) {
			series = append(series, *traffic[node])
		}
		charts = append(charts, template.HTML(chart.Line("Mesh traffic per device (rx + tx, mesh.rx_bytes and tx_bytes)", "MiB/s", series, nil, nil, chart.Axis{}))) //nolint:gosec // generated SVG
	}
	if pathsRec != nil {
		for _, pair := range sortedKeys(pathsRec.Attrs) {
			from, to, _ := strings.Cut(pair, ">")
			f := strings.Fields(pathsRec.Attrs[pair])
			for len(f) < 3 {
				f = append(f, "")
			}
			pairs = append(pairs, pairView{From: from, To: to, Class: f[0], Endpoint: f[1], Handshake: f[2]})
		}
	}
	return charts, pairs, join
}

func ms(v string) string {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return v
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1f s", float64(n)/1000)
	}
	return fmt.Sprintf("%d ms", n)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
