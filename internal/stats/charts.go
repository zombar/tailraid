package stats

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/chart"
)

// Chart is one drawn chart of the page (docs/17 §4): the SVG and its caption.
type Chart struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	SVG     string `json:"svg"`
	Caption string `json:"caption"`
}

// Downsample folds points to at most n buckets over [from, to) by the mean of each
// bucket (I-196); fewer points pass through unchanged. x is seconds since from.
func Downsample(points [][2]float64, n int) [][2]float64 {
	if n <= 0 || len(points) <= n {
		return points
	}
	minX, maxX := points[0][0], points[0][0]
	for _, p := range points {
		minX, maxX = min(minX, p[0]), max(maxX, p[0])
	}
	width := (maxX - minX) / float64(n)
	if width <= 0 { // every point at one x: one mean point
		var sum float64
		for _, p := range points {
			sum += p[1]
		}
		return [][2]float64{{minX, sum / float64(len(points))}}
	}
	sumX, sumY := make([]float64, n), make([]float64, n)
	cnt := make([]int, n)
	for _, p := range points {
		b := min(int((p[0]-minX)/width), n-1)
		sumX[b] += p[0]
		sumY[b] += p[1]
		cnt[b]++
	}
	out := make([][2]float64, 0, n)
	for b := range n {
		if cnt[b] > 0 {
			out = append(out, [2]float64{sumX[b] / float64(cnt[b]), sumY[b] / float64(cnt[b])})
		}
	}
	return out
}

// Charts draws the page's charts from the rows of [from, to): the cluster charts from the
// cluster rows, the node charts from the rows of node (every node as one series each when
// node is "all"). Markers come from the cluster rows: a leader change, a device going dead
// or live, a repair pass starting and ending.
func Charts(rows []Row, from, to time.Time, node string) []Chart {
	ax := chart.Axis{Clock: true, Start: from, Span: to.Sub(from).Seconds()}
	x := func(t int64) float64 { return float64(t - from.Unix()) }
	span := to.Sub(from).Seconds()
	var cluster []Row
	byNode := map[string][]Row{}
	var names []string
	for _, r := range rows {
		if r.C != nil {
			cluster = append(cluster, r)
			continue
		}
		if _, ok := byNode[r.N]; !ok {
			names = append(names, r.N)
		}
		byNode[r.N] = append(byNode[r.N], r)
	}
	sort.Strings(names)
	marks := markers(cluster, x)
	var out []Chart
	add := func(id, title, caption, svg string) {
		if svg != "" {
			out = append(out, Chart{ID: id, Title: title, SVG: svg, Caption: caption})
		}
	}
	// a cluster metric as a series
	cs := func(name string, f func(*Cluster) float64) chart.Series {
		pts := make([][2]float64, 0, len(cluster))
		for _, r := range cluster {
			pts = append(pts, [2]float64{x(r.T), f(r.C)})
		}
		return chart.Series{Name: name, Points: Downsample(pts, chart.Points)}
	}
	line := func(id, title, y string, ss []chart.Series) {
		for _, s := range ss {
			if len(s.Points) > 0 {
				add(id, title, y, chart.Line(title, y, ss, marks, nil, ax))
				return
			}
		}
	}
	if len(cluster) > 0 {
		line("stored", "Stored vs logical bytes (dedup)", "bytes", []chart.Series{cs("stored bytes", func(c *Cluster) float64 { return c.Stored }), cs("logical bytes", func(c *Cluster) float64 { return c.Logical })})
		line("chunks", "Chunks", "chunks", []chart.Series{cs("live chunks", func(c *Cluster) float64 { return c.Chunks })})
		line("backlog", "Repair backlog", "chunks", []chart.Series{
			cs("degraded", func(c *Cluster) float64 { return c.Degraded }), cs("repair pending", func(c *Cluster) float64 { return c.Pending }),
			cs("stale policy", func(c *Cluster) float64 { return c.Stale }), cs("unreadable", func(c *Cluster) float64 { return c.Unreadable }),
		})
		line("moved", "Repair bytes moved", "MiB/s", []chart.Series{cs("bytes moved MiB/s", func(c *Cluster) float64 { return c.RepairMoved / (1 << 20) })})
		add("liveness", "Node liveness as seen by the leader", "live · dead · suspect", lanes(cluster, span, x, ax))
		add("spread", "Data spread across disks", "per target, grouped by host", spread(cluster, span, x, ax))
		line("slots", "Shard slots per node", "slots", slotSeries(cluster, x))
	}
	// node charts: one series per metric for one node, or one series per node for a metric
	ns := func(id, title, y string, metrics ...[2]string) {
		var ss []chart.Series
		if node == "all" {
			m := metrics[0]
			for _, n := range names {
				ss = append(ss, chart.Series{Name: n, Points: Downsample(metricPoints(byNode[n], m[0], x), chart.Points)})
			}
		} else {
			for _, m := range metrics {
				ss = append(ss, chart.Series{Name: m[1], Points: Downsample(metricPoints(byNode[node], m[0], x), chart.Points)})
			}
		}
		line(id, title, y, ss)
	}
	ns("throughput", "Client throughput", "MiB/s", [2]string{"bytes_w", "write MiB/s"}, [2]string{"bytes_r", "read MiB/s"})
	ns("tailnet", "Network (mesh)", "MiB/s", [2]string{"mesh_rx", "mesh rx MiB/s"}, [2]string{"mesh_tx", "mesh tx MiB/s"}, [2]string{"relayed", "relayed MiB/s"})
	ns("net", "Network (all interfaces)", "MiB/s", [2]string{"net_rx", "rx MiB/s"}, [2]string{"net_tx", "tx MiB/s"})
	// the mesh's history (docs/17 §4, I-332): the path to each peer as lanes and the round
	// trip to each as a line; every pair, and each member's mean, with node "all"
	if node == "all" {
		add("paths", MeshPathsTitle(node), "lan · wan · relay · none, every pair", pathLanes(MeshPathsTitle(node), pairLanes(names, byNode), span, x, ax))
		ss := make([]chart.Series, 0, len(names))
		for _, n := range names {
			if pts := meanRTT(byNode[n], x); len(pts) > 0 { // a member without round trips has no series
				ss = append(ss, chart.Series{Name: n, Points: Downsample(pts, chart.Points)})
			}
		}
		line("rtt", MeshRTTTitle(node), "ms", ss)
	} else {
		rows := byNode[node]
		lanesOf := map[string][]Row{}
		for _, peer := range peersOf(rows) {
			lanesOf[peer] = rows
		}
		add("paths", MeshPathsTitle(node), "lan · wan · relay · none", pathLanes(MeshPathsTitle(node), lanesOf, span, x, ax))
		peers := peersOf(rows)
		ss := make([]chart.Series, 0, len(peers))
		for _, peer := range peers {
			ss = append(ss, chart.Series{Name: peer, Points: Downsample(metricPoints(rows, "rtt."+peer, x), chart.Points)})
		}
		line("rtt", MeshRTTTitle(node), "ms", ss)
	}
	ns("cpu", "CPU", "%", [2]string{"cpu", "process % of one CPU"}, [2]string{"load1", "load (1 min)"})
	ns("memory", "Memory", "bytes", [2]string{"rss", "resident set"}, [2]string{"mem_avail", "memory available"})
	ns("pressure", "Memory pressure", "%", [2]string{"mem_pressure", "memory pressure (PSI some, avg10 %)"})
	ns("diskfree", "Data disk free", "bytes", [2]string{"disk_free", "data disk free"})
	ns("diskio", "Disk throughput", "MiB/s", [2]string{"disk_w", "disk write MiB/s"}, [2]string{"disk_r", "disk read MiB/s"})
	ns("wal", "Overlay WAL depth", "bytes", [2]string{"wal", "overlay WAL bytes"}, [2]string{"dirty", "dirty bytes"}, [2]string{"plan", "plan bytes"}, [2]string{"flush", "flush buffers"})
	ns("process", "Process memory", "bytes", [2]string{"heap", "heap in use"}, [2]string{"sys", "sys"})
	ns("readahead", "Readahead", "chunks/s", [2]string{"ra_fetched", "fetched chunks/s"}, [2]string{"ra_joined", "joined READs/s"}, [2]string{"prefetching", "prefetching"})
	return out
}

// metricPoints is one metric of one node over time, in MiB/s for the byte rates and
// percent for cpu; rows without the metric leave a gap.
func metricPoints(rows []Row, metric string, x func(int64) float64) [][2]float64 {
	scale := 1.0
	switch metric {
	case "bytes_w", "bytes_r", "mesh_rx", "mesh_tx", "relayed", "net_rx", "net_tx", "disk_w", "disk_r":
		scale = 1.0 / (1 << 20)
	case "cpu":
		scale = 100
	}
	var pts [][2]float64
	for _, r := range rows {
		if v, ok := r.M[metric]; ok {
			pts = append(pts, [2]float64{x(r.T), v * scale})
		}
	}
	return pts
}

// markers derives the event lines from the cluster rows.
func markers(cluster []Row, x func(int64) float64) []chart.Marker {
	var marks []chart.Marker
	var prev *Row
	for i := range cluster {
		r := &cluster[i]
		if prev != nil {
			if r.Term != prev.Term {
				marks = append(marks, chart.Marker{X: x(r.T), Label: fmt.Sprintf("leader %s (term %d)", r.By, r.Term)})
			}
			for _, name := range sortedKeys(r.C.Live) {
				was, now := prev.C.Live[name], r.C.Live[name]
				switch {
				case was != 0 && now == 0:
					marks = append(marks, chart.Marker{X: x(r.T), Label: name + " dead"})
				case was == 0 && now != 0:
					marks = append(marks, chart.Marker{X: x(r.T), Label: name + " live"})
				}
			}
			if prev.C.RepairMoved == 0 && r.C.RepairMoved > 0 {
				marks = append(marks, chart.Marker{X: x(r.T), Label: "repair pass start"})
			}
			if prev.C.RepairMoved > 0 && r.C.RepairMoved == 0 {
				marks = append(marks, chart.Marker{X: x(r.T), Label: "repair pass end"})
			}
		}
		prev = r
	}
	if len(marks) > 24 { // a busy range keeps its first and last events readable
		marks = append(marks[:12], marks[len(marks)-12:]...)
	}
	return marks
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lanes is every device's liveness over time.
func lanes(cluster []Row, span float64, x func(int64) float64, ax chart.Axis) string {
	cells := map[string][]chart.Cell{}
	var names []string
	seen := map[string]bool{}
	step := max(1, len(cluster)/chart.Points)
	for i := 0; i < len(cluster); i += step {
		r := cluster[i]
		for _, name := range sortedKeys(r.C.Live) {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			st := chart.OK
			switch r.C.Live[name] {
			case 0:
				st = chart.Bad
			case 2:
				st = chart.Warn
			}
			cells[name] = append(cells[name], chart.Cell{X: x(r.T), Status: st})
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return chart.Lanes("Node liveness as seen by the leader", span, names, cells, ax)
}

// spread is the disk-spread lanes over the cluster rows that carry targets.
func spread(cluster []Row, span float64, x func(int64) float64, ax chart.Axis) string {
	var xs []float64
	var samples []chart.SpreadSample
	step := max(1, len(cluster)/chart.Points)
	for i := 0; i < len(cluster); i += step {
		r := cluster[i]
		if len(r.C.Targets) == 0 {
			continue
		}
		xs = append(xs, x(r.T))
		samples = append(samples, chart.SpreadSample{Targets: r.C.Targets, Chunks: int(r.C.Chunks), DomainSkew: int(r.C.DomainSkew)})
	}
	if len(samples) == 0 {
		return ""
	}
	return chart.Spread(span, xs, samples, ax)
}

// slotSeries is the slots held per host over time, from the targets.
func slotSeries(cluster []Row, x func(int64) float64) []chart.Series {
	per := map[string][][2]float64{}
	var hosts []string
	for _, r := range cluster {
		sum := map[string]int{}
		for _, t := range r.C.Targets {
			sum[t.Host] += t.Slots
		}
		for _, h := range sortedKeys(sum) {
			if _, ok := per[h]; !ok {
				hosts = append(hosts, h)
			}
			per[h] = append(per[h], [2]float64{x(r.T), float64(sum[h])})
		}
	}
	sort.Strings(hosts)
	ss := make([]chart.Series, 0, len(hosts))
	for _, h := range hosts {
		ss = append(ss, chart.Series{Name: h, Points: Downsample(per[h], chart.Points)})
	}
	return ss
}

// MeshPathsTitle and MeshRTTTitle name the mesh charts of a node, or of every pair.
func MeshPathsTitle(node string) string {
	if node == "all" {
		return "Mesh paths (every pair)"
	}
	return "Mesh paths from " + node
}

func MeshRTTTitle(node string) string {
	if node == "all" {
		return "Mesh round trip (mean per member)"
	}
	return "Mesh round trip from " + node
}

// peersOf is every peer a node's rows carry a path for, sorted.
func peersOf(rows []Row) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		for k := range r.M {
			if peer, ok := strings.CutPrefix(k, "path."); ok && !seen[peer] {
				seen[peer] = true
				out = append(out, peer)
			}
		}
	}
	sort.Strings(out)
	return out
}

// pairLanes is one lane per ordered pair "a → b" over the rows of a (the lane's key is
// the peer's name, the lane's rows the node's).
func pairLanes(names []string, byNode map[string][]Row) map[string][]Row {
	out := map[string][]Row{}
	for _, n := range names {
		for _, peer := range peersOf(byNode[n]) {
			// the lane reads rows of n under the key "path.<peer>", so the lane carries
			// the rows re-keyed to its own name
			rows := make([]Row, 0, len(byNode[n]))
			for _, r := range byNode[n] {
				if v, ok := r.M["path."+peer]; ok {
					rows = append(rows, Row{T: r.T, N: r.N, M: map[string]float64{"path." + n + " → " + peer: v}})
				}
			}
			out[n+" → "+peer] = rows
		}
	}
	return out
}

// pathLanes draws one lane per key of lanes, its cells the class codes of path.<key>
// in that lane's rows, sampled to the chart's width; "" without a lane.
func pathLanes(title string, lanes map[string][]Row, span float64, x func(int64) float64, ax chart.Axis) string {
	cells := map[string][]chart.Cell{}
	var names []string
	for name, rows := range lanes {
		key := "path." + name
		step := max(1, len(rows)/chart.Points)
		for i := 0; i < len(rows); i += step {
			v, ok := rows[i].M[key]
			if !ok {
				continue
			}
			st := chart.Bad
			switch PathClass(v) {
			case "lan":
				st = chart.OK
			case "wan":
				st = chart.Warn
			case "relay":
				st = chart.Off
			}
			cells[name] = append(cells[name], chart.Cell{X: x(rows[i].T), Status: st})
		}
		if len(cells[name]) > 0 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return chart.Lanes(title, span, names, cells, ax)
}

// meanRTT is a node's mean round trip over its peers, per row.
func meanRTT(rows []Row, x func(int64) float64) [][2]float64 {
	var pts [][2]float64
	for _, r := range rows {
		var sum float64
		n := 0
		for k, v := range r.M {
			if strings.HasPrefix(k, "rtt.") {
				sum += v
				n++
			}
		}
		if n > 0 {
			pts = append(pts, [2]float64{x(r.T), sum / float64(n)})
		}
	}
	return pts
}
