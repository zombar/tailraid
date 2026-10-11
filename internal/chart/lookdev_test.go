package chart

import (
	"math"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The lookdev of the Stats page (docs/assets/lookdev/stats.html, docs/13 §Design) carries
// this package's rendering of one fixed six-hour data set, so the signed-off markup and the
// code agree. Set UPDATE_LOOKDEV=1 to rewrite the page's chart blocks from the package.

const lookdevPage = "../../docs/assets/lookdev/stats.html"

// lookdevCharts renders the data set: five hosts, lumen dead for twenty minutes, a leader
// change, a repair pass, a disk added to nomad, a write burst and a read burst, at one
// row a minute (the lanes and the spread at one row every three minutes).
func lookdevCharts() []struct{ id, svg string } {
	const n = 180
	const span = 21600.0 // 08:00 to 14:00 UTC
	ax := Axis{Clock: true, Start: time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)}
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // a fixed data set
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = span * float64(i) / float64(n-1)
	}
	at := func(h, m int) float64 { return float64((h-8)*3600 + m*60) }
	in := func(x, a, b float64) bool { return x >= a && x < b }
	noise := func(a float64) float64 { return (rng.Float64()*2 - 1) * a }
	pts := func(f func(x float64) float64) [][2]float64 {
		p := make([][2]float64, n)
		for i, x := range xs {
			p[i] = [2]float64{x, math.Max(0, f(x))}
		}
		return p
	}
	marks := []Marker{{at(9, 12), "leader pike (term 13)"}, {at(10, 10), "lumen dead"}, {at(10, 30), "lumen live"}, {at(11, 0), "repair pass 4 start"}, {at(11, 40), "repair pass 4 end"}, {at(12, 0), "disk added nomad/3"}}
	const gib = 1 << 30
	const mib = 1 << 20
	step := func(a, b float64) func(float64) float64 {
		return func(x float64) float64 {
			if in(x, a, b) {
				return 1
			}
			return 0
		}
	}
	wburst, rburst, pass := step(at(8, 20), at(8, 50)), step(at(13, 0), at(13, 30)), step(at(11, 0), at(11, 40))
	degraded := func(x float64) float64 {
		switch {
		case x < at(10, 10):
			return 0
		case x < at(11, 0):
			return 37
		case x < at(11, 40):
			return 37 * (at(11, 40) - x) / (40 * 60)
		}
		return 0
	}
	written := func(x float64) float64 { return 1.2 * gib * math.Min(1, math.Max(0, (x-at(8, 20))/(30*60))) }
	logical := func(x float64) float64 { return 41.3*gib + written(x) }
	var out []struct{ id, svg string }
	add := func(id, svg string) { out = append(out, struct{ id, svg string }{id, svg}) }
	add("stored", Line("Stored vs logical bytes (dedup)", "bytes", []Series{
		{Name: "stored bytes", Points: pts(func(x float64) float64 { return logical(x) * 1.354 })},
		{Name: "logical bytes", Points: pts(logical)},
	}, marks, nil, ax))
	add("chunks", Line("Chunks", "chunks", []Series{
		{Name: "live chunks", Points: pts(func(x float64) float64 { return 48211 + written(x)/(4*mib) })},
		{Name: "tombstoned", Points: pts(func(x float64) float64 { return 212 + 1300*pass(x)*math.Sin((x-at(11, 0))/(40*60)*math.Pi) + noise(6) })},
	}, marks, nil, ax))
	add("backlog", Line("Repair backlog", "chunks", []Series{
		{Name: "degraded", Points: pts(degraded)},
		{Name: "repair pending", Points: pts(func(x float64) float64 {
			if x < at(10, 20) {
				return 0
			}
			return degraded(x)
		})},
		{Name: "stale policy", Points: pts(func(float64) float64 { return 0 })},
		{Name: "unreadable", Points: pts(func(float64) float64 { return 0 })},
	}, marks, nil, ax))
	add("moved", Line("Repair bytes moved", "MiB/s", []Series{
		{Name: "bytes moved MiB/s", Points: pts(func(x float64) float64 { return pass(x) * (12 + noise(4)) })},
	}, marks, nil, ax))
	var xs3 []float64
	for i, x := range xs {
		if i%3 == 0 || i == n-1 {
			xs3 = append(xs3, x)
		}
	}
	names := []string{"atlas", "pike", "lumen", "nomad", "fern"}
	cells := map[string][]Cell{}
	for _, nm := range names {
		for _, x := range xs3 {
			c := OK
			if nm == "lumen" && in(x, at(10, 10), at(10, 30)) {
				c = Bad
			}
			if nm == "nomad" && in(x, at(12, 0), at(12, 4)) {
				c = Warn
			}
			cells[nm] = append(cells[nm], Cell{x, c})
		}
	}
	add("liveness", Lanes("Node liveness as seen by the leader", span, names, cells, ax))
	type tg struct {
		name, host string
		capacity   uint64
		dev        float64
	}
	base := []tg{
		{"atlas/1", "atlas", 500 * gib, 0.008},
		{"atlas/2", "atlas", 2048 * gib, 0.003},
		{"atlas/3", "atlas", 2048 * gib, -0.01},
		{"pike/1", "pike", 500 * gib, 0.032},
		{"pike/2", "pike", 1024 * gib, 0.011},
		{"pike/3", "pike", 1024 * gib, -0.019},
		{"lumen/1", "lumen", 500 * gib, 0.014},
		{"lumen/2", "lumen", 2048 * gib, -0.002},
		{"lumen/3", "lumen", 2048 * gib, -0.007},
		{"nomad/1", "nomad", 250 * gib, -0.032},
		{"nomad/2", "nomad", 500 * gib, -0.03},
		{"nomad/3", "nomad", 500 * gib, 0},
	}
	samples := make([]SpreadSample, len(xs3))
	slotsPer := map[string][][2]float64{}
	for i, x := range xs3 {
		chunks := int(48211 + written(x)/(4*mib))
		var ts []Target
		for _, b := range base {
			if b.name == "nomad/3" && x < at(12, 0) {
				continue
			}
			ts = append(ts, Target{Name: b.name, Host: b.host, Capacity: b.capacity, Live: !(b.host == "lumen" && in(x, at(10, 10), at(10, 30))), Active: true})
		}
		exp := ExpectedShares(ts, chunks)
		fill := math.Min(1, math.Max(0, (x-at(12, 0))/(2*3600))) // nomad/3 fills over two hours
		for j := range ts {
			b := base[0]
			for _, c := range base {
				if c.name == ts[j].Name {
					b = c
				}
			}
			d := b.dev
			if b.name == "nomad/3" {
				d = -(1 - fill)
			} else if x >= at(12, 0) {
				d += 0.06 * (1 - fill) // the others hold nomad/3's share until it fills
			}
			ts[j].Slots = int(exp[j] * (1 + d))
			slotsPer[b.host] = append(slotsPer[b.host], [2]float64{x, 0})
		}
		skew := 0
		if in(x, at(12, 0), at(12, 20)) {
			skew = int(420 * (at(12, 20) - x) / (20 * 60))
		}
		samples[i] = SpreadSample{Targets: ts, Chunks: chunks, DomainSkew: skew}
		for _, t := range ts {
			p := slotsPer[t.Host]
			p[len(p)-1][1] += float64(t.Slots)
		}
	}
	add("spread", Spread(span, xs3, samples, ax))
	var slotSeries []Series
	for _, h := range []string{"atlas", "pike", "lumen", "nomad"} {
		var p [][2]float64
		for _, q := range slotsPer[h] {
			if len(p) > 0 && p[len(p)-1][0] == q[0] {
				p[len(p)-1][1] += q[1]
				continue
			}
			p = append(p, q)
		}
		slotSeries = append(slotSeries, Series{Name: h, Points: p})
	}
	add("slots", Line("Shard slots per node", "slots", slotSeries, marks, nil, ax))
	add("throughput", Line("Client throughput", "MiB/s", []Series{
		{Name: "write MiB/s", Points: pts(func(x float64) float64 { return wburst(x)*(40+noise(6)) + math.Abs(noise(0.4)) })},
		{Name: "read MiB/s", Points: pts(func(x float64) float64 { return rburst(x)*(90+noise(12)) + math.Abs(noise(0.6)) })},
	}, marks, nil, ax))
	add("tailnet", Line("Network (tailnet)", "MiB/s", []Series{
		{Name: "tailnet rx MiB/s", Points: pts(func(x float64) float64 {
			return rburst(x)*(60+noise(8)) + pass(x)*(9+noise(3)) + wburst(x)*(3+noise(1)) + math.Abs(noise(0.3))
		})},
		{Name: "tailnet tx MiB/s", Points: pts(func(x float64) float64 {
			return wburst(x)*(66+noise(8)) + pass(x)*(8+noise(3)) + rburst(x)*(2+noise(1)) + math.Abs(noise(0.3))
		})},
	}, marks, nil, ax))
	add("net", Line("Network (all interfaces)", "MiB/s", []Series{
		{Name: "rx MiB/s", Points: pts(func(x float64) float64 {
			return rburst(x)*(60+noise(8)) + pass(x)*(9+noise(3)) + wburst(x)*(44+noise(4)) + 1.5 + math.Abs(noise(1))
		})},
		{Name: "tx MiB/s", Points: pts(func(x float64) float64 {
			return wburst(x)*(66+noise(8)) + pass(x)*(8+noise(3)) + rburst(x)*(92+noise(10)) + 0.8 + math.Abs(noise(0.6))
		})},
	}, marks, nil, ax))
	add("cpu", Line("CPU", "%", []Series{
		{Name: "process % of one CPU", Points: pts(func(x float64) float64 { return 6 + wburst(x)*55 + rburst(x)*48 + pass(x)*22 + noise(3) })},
		{Name: "machine % busy", Points: pts(func(x float64) float64 { return 14 + wburst(x)*40 + rburst(x)*30 + pass(x)*12 + noise(4) })},
		{Name: "load (1 min)", Points: pts(func(x float64) float64 { return 0.8 + wburst(x)*1.9 + rburst(x)*1.4 + pass(x)*0.7 + noise(0.2) })},
	}, marks, nil, ax))
	add("memory", Line("Memory", "bytes", []Series{
		{Name: "resident set", Points: pts(func(x float64) float64 {
			return 410*mib + wburst(x)*230*mib + rburst(x)*180*mib + pass(x)*90*mib + noise(8*mib)
		})},
		{Name: "memory available", Points: pts(func(x float64) float64 { return 2.1*gib - wburst(x)*0.4*gib - rburst(x)*0.3*gib + noise(30*mib) })},
	}, marks, nil, ax))
	add("pressure", Line("Memory pressure", "%", []Series{
		{Name: "memory pressure (PSI some, avg10 %)", Points: pts(func(x float64) float64 { return 0.2 + rburst(x)*3.8 + wburst(x)*1.1 + math.Abs(noise(0.3)) })},
	}, marks, nil, ax))
	add("diskfree", Line("Data disk free", "bytes", []Series{
		{Name: "data disk free", Points: pts(func(x float64) float64 { return 512*gib - written(x)*1.67/4 })},
	}, marks, nil, ax))
	add("diskio", Line("Disk throughput", "MiB/s", []Series{
		{Name: "disk write MiB/s", Points: pts(func(x float64) float64 { return wburst(x)*(22+noise(5)) + pass(x)*(6+noise(2)) + math.Abs(noise(0.5)) })},
		{Name: "disk read MiB/s", Points: pts(func(x float64) float64 { return rburst(x)*(31+noise(6)) + pass(x)*(5+noise(2)) + math.Abs(noise(0.5)) })},
	}, marks, nil, ax))
	saw := func(x float64) float64 { return math.Mod(x, 90) / 90 }
	add("wal", Line("Overlay WAL depth", "bytes", []Series{
		{Name: "overlay WAL bytes", Points: pts(func(x float64) float64 { return wburst(x) * (20 + 100*saw(x)) * mib })},
		{Name: "dirty bytes", Points: pts(func(x float64) float64 { return wburst(x) * (10 + 60*saw(x)) * mib })},
		{Name: "plan bytes", Points: pts(func(x float64) float64 { return wburst(x) * (30 + 20*saw(x+30)) * mib })},
		{Name: "flush buffers", Points: pts(func(x float64) float64 { return wburst(x) * (38 + noise(6)) * mib })},
	}, marks, nil, ax))
	add("process", Line("Process memory", "bytes", []Series{
		{Name: "heap in use", Points: pts(func(x float64) float64 {
			return 92*mib + wburst(x)*70*mib + rburst(x)*60*mib + pass(x)*28*mib + noise(5*mib)
		})},
		{Name: "sys", Points: pts(func(x float64) float64 { return 184*mib + wburst(x)*80*mib + rburst(x)*70*mib + pass(x)*30*mib })},
	}, marks, nil, ax))
	add("readahead", Line("Readahead", "chunks/s", []Series{
		{Name: "fetched chunks/s", Points: pts(func(x float64) float64 { return rburst(x) * (22 + noise(3)) })},
		{Name: "joined READs/s", Points: pts(func(x float64) float64 { return rburst(x) * (20 + noise(3)) })},
		{Name: "prefetching", Points: pts(func(x float64) float64 { return rburst(x) * (3 + math.Round(noise(1))) })},
	}, marks, nil, ax))
	return out
}

var chartBlock = regexp.MustCompile(`(?s)<svg class="chart".*?</svg>`)

// Invariant: I-197
func TestChart_LookdevDataSetMatchesStatsPage(t *testing.T) {
	page, err := os.ReadFile(lookdevPage)
	if err != nil {
		t.Fatal(err)
	}
	want := lookdevCharts()
	blocks := chartBlock.FindAllStringIndex(string(page), -1)
	if len(blocks) != len(want) {
		t.Fatalf("the page has %d chart blocks, the data set %d", len(blocks), len(want))
	}
	if os.Getenv("UPDATE_LOOKDEV") != "" {
		var b strings.Builder
		last := 0
		for i, bl := range blocks {
			b.WriteString(string(page[last:bl[0]]))
			b.WriteString(want[i].svg)
			last = bl[1]
		}
		b.WriteString(string(page[last:]))
		if err := os.WriteFile(lookdevPage, []byte(b.String()), 0o644); err != nil { //nolint:gosec // the lookdev page
			t.Fatal(err)
		}
		t.Log("rewrote the lookdev's chart blocks")
	}
	for i, bl := range blocks {
		if got := string(page[bl[0]:bl[1]]); got != want[i].svg {
			t.Errorf("chart %d (%s) differs from the package's rendering; run with UPDATE_LOOKDEV=1 to rewrite the page", i, want[i].id)
		}
	}
}
