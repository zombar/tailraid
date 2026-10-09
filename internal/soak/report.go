package soak

import (
	"fmt"
	"html/template"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Render writes the self-contained HTML report for a run (docs/09 §6).
func Render(w io.Writer, s Summary) error {
	data := buildReport(s)
	return reportTmpl.Execute(w, data)
}

type reportData struct {
	Title      string
	Env        string
	Profile    string
	Seed       int64
	Flags      string
	Policy     string // "2+1, chunk 1.0 MiB, 4 nodes"
	Started    string
	Duration   string
	Verdict    string // PASS | FAIL | PARTIAL
	Failed     int
	Checks     int
	Skipped    []string
	Nodes      []nodeView
	Phases     []phaseView
	Issues     []issueView
	Notes      []noteView
	Dumps      []dumpView
	Latency    []latencyView
	Totals     []kv
	Coverage   []CoverageRow
	Charts     []template.HTML
	Disks      []diskView // the targets at the end of the run (docs/14), empty without disks
	PeakSkew   int        // most chunks above the host cap in any sample
	Generated  string
	Throughput string
	Mark       template.HTML // the logo mark, inline
	Favicon    string        // the mark, URL-encoded for a data: favicon
}

// logoMark is docs/assets/logo-mark.svg (currentColor ink, blue parity bar).
const logoMark = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="-50 -50 100 100"><g transform="rotate(32)"><rect x="-34" y="-40" width="16" height="80" rx="1" fill="currentColor"/><path stroke="#2563eb" stroke-width="3.5" stroke-linecap="square" stroke-linejoin="miter" fill="none" d="M -9 -20 V -40 H 9 V -20 M -9 -10 V 10 M 9 -10 V 10 M -9 20 V 40 H 9 V 20"/><rect x="18" y="-40" width="16" height="80" rx="1" fill="currentColor"/></g></svg>`

type nodeView struct {
	NodeRef
	Version string
}

type phaseView struct {
	Name   string
	Status string
	Took   string
	Checks []checkView
	Reason string
}

type checkView struct {
	Name  string
	Cites string
	OK    bool
	Text  string
}

type issueView struct {
	Record
	Where string // "results.jsonl:123"
}

type noteView struct {
	Phase string
	Text  string
}

// dumpView is one node's goroutine dump taken after a failed phase.
type dumpView struct {
	Phase string
	Node  string
	Lines int
	Text  string
}

type latencyView struct {
	Phase  string
	Kind   string
	Window string
	N      int
	P50    string
	P99    string
	Max    string
}

type kv struct{ K, V string }

func buildReport(s Summary) reportData {
	d := reportData{
		Title: "tailraid soak report", Env: s.Env, Profile: s.Profile, Seed: s.Seed, Flags: strings.Join(s.Flags, ", "),
		Started: s.Started.UTC().Format(time.RFC3339), Duration: s.Duration.Round(time.Second).String(),
		Generated: time.Now().UTC().Format(time.RFC3339), Failed: s.Failed, Coverage: s.Coverage,
	}
	if d.Flags == "" {
		d.Flags = "none"
	}
	d.Policy = fmt.Sprintf("%d+%d, chunk %s, %d nodes", s.K, s.M, humanBytes(float64(s.ChunkSize)), s.NodeCount)
	for _, n := range s.Nodes {
		d.Nodes = append(d.Nodes, nodeView{NodeRef: n, Version: s.Versions[n.Name]})
	}
	for _, ph := range s.Phases {
		pv := phaseView{Name: ph.Name, Took: ph.Took.Round(time.Millisecond).String(), Reason: ph.Reason}
		switch {
		case ph.Skipped:
			pv.Status = "SKIPPED"
			d.Skipped = append(d.Skipped, ph.Name)
		case ph.Failed > 0:
			pv.Status = "FAIL"
		default:
			pv.Status = "PASS"
		}
		for i := range ph.Checks {
			c := &ph.Checks[i]
			ok := c.OK == nil || *c.OK
			pv.Checks = append(pv.Checks, checkView{Name: c.Name, Cites: c.Cites, OK: ok, Text: c.Text})
			d.Checks++
			if !ok {
				d.Issues = append(d.Issues, issueView{Record: *c, Where: fmt.Sprintf("results.jsonl:%d", c.Line)})
			}
		}
		d.Phases = append(d.Phases, pv)
	}
	switch {
	case d.Failed > 0:
		d.Verdict = "FAIL"
	case s.Partial:
		d.Verdict = "PARTIAL"
	default:
		d.Verdict = "PASS"
	}
	for i := range s.Records {
		rec := &s.Records[i]
		switch {
		case rec.Kind == "event" && rec.Name == "note":
			d.Notes = append(d.Notes, noteView{Phase: rec.Phase, Text: rec.Text})
		case rec.Kind == "dump":
			d.Dumps = append(d.Dumps, dumpView{Phase: rec.Phase, Node: rec.Node, Lines: strings.Count(rec.Text, "\n"), Text: rec.Text})
		}
	}
	for _, row := range s.Latency {
		w := "whole phase"
		if row.UnderFaults {
			w = "under faults"
		}
		d.Latency = append(d.Latency, latencyView{
			Phase: row.Phase, Kind: row.Kind, Window: w, N: row.N,
			P50: row.P50.Round(100 * time.Microsecond).String(), P99: row.P99.Round(100 * time.Microsecond).String(), Max: row.Max.Round(100 * time.Microsecond).String(),
		})
	}
	t := s.Totals
	d.Totals = []kv{
		{"written", fmt.Sprintf("%s in %d ops", humanBytes(float64(t.BytesWritten)), t.Writes)},
		{"read", fmt.Sprintf("%s in %d ops", humanBytes(float64(t.BytesRead)), t.Reads)},
		{"dedup ratio", fmt.Sprintf("%.2fx", t.DedupRatio)},
		{"deepest overlay WAL", humanBytes(float64(t.MaxWALBytes))},
		{"peak flush buffers", humanBytes(float64(t.MaxFlush))},
		{"peak heap in use", humanBytes(float64(t.MaxHeap))},
		{"peak resident set", humanBytes(float64(t.MaxRSS))},
		{"peak process CPU", fmt.Sprintf("%.0f %% of one CPU", t.MaxCPUPct)},
		{"least free disk on the sampled node", humanBytes(float64(t.MinDiskFree))},
		{"repair", fmt.Sprintf("%d scanned, %d repaired, %d partial, %d unrepairable, %d migrated, %d deferred, %d scrubbed, %d corrupt, %s moved, %d errors",
			t.Repair.Scanned, t.Repair.Repaired, t.Repair.Partial, t.Repair.Unrepairable, t.Repair.Migrated, t.Repair.Deferred, t.Repair.Scrubbed, t.Repair.ScrubBad, humanBytes(float64(t.Repair.BytesMoved)), t.Repair.Errors)},
		{"gc", fmt.Sprintf("%d chunk entries collected, %d shards deleted, %d deletes refused", t.GC.Victims, t.GC.Deleted, t.GC.Refused)},
	}
	d.Charts, d.Throughput = charts(s)
	var spread []*Sample
	for i := range s.Records {
		if s.Records[i].Kind == "sample" && s.Records[i].S != nil {
			spread = append(spread, s.Records[i].S)
		}
	}
	d.Disks, d.PeakSkew = disksTable(spread)
	d.Mark = template.HTML(logoMark) //nolint:gosec // constant SVG
	d.Favicon = url.PathEscape(strings.Replace(logoMark, `fill="currentColor"`, `fill="#0f172a"`, 2))
	return d
}

// placementChart draws the large file's shard slots per node (phase 12): data and parity
// slots held, against the node's capacity share of all its slots.
func placementChart(rec *Record) string {
	names := make([]string, 0, len(rec.Attrs))
	for n := range rec.Attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	data, parity, expected := series{name: "data slots"}, series{name: "parity slots"}, series{name: "capacity share", color: "#94a3b8"}
	for _, n := range names {
		var d, p, e float64
		_, _ = fmt.Sscanf(rec.Attrs[n], "%f/%f/%f", &d, &p, &e)
		data.points = append(data.points, [2]float64{0, d})
		parity.points = append(parity.points, [2]float64{0, p})
		expected.points = append(expected.points, [2]float64{0, e})
	}
	return barChart("Large file: shard slots per node ("+rec.Text+")", names, []series{data, parity, expected})
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B", b)
}

// charts derives every figure from the sample, event and phase records.
func charts(s Summary) ([]template.HTML, string) {
	var samples []Record
	var marks []marker
	var bands []band
	var placement *Record // the large file's per-node slot counts (phase 12)
	x := func(rec *Record) float64 { return rec.T.Sub(s.Started).Seconds() }
	for i := range s.Records {
		rec := &s.Records[i]
		switch rec.Kind {
		case "sample":
			if rec.S != nil {
				samples = append(samples, *rec)
			}
		case "event":
			if rec.Name == placementEvent {
				placement = rec
				continue
			}
			if rec.Name != "note" && rec.Name != "baseline written" {
				marks = append(marks, marker{x: x(rec), label: strings.TrimSpace(rec.Name + " " + rec.Text)})
			}
		case "phase":
			if rec.Text == "start" {
				if n := len(bands); n > 0 {
					bands[n-1].x1 = x(rec)
				}
				bands = append(bands, band{x0: x(rec), label: rec.Name})
			}
		}
	}
	if n := len(bands); n > 0 {
		bands[n-1].x1 = s.Duration.Seconds()
	}
	var out []template.HTML
	// throughput: bytes per second between consecutive samples; the extra samples at
	// phase boundaries sit too close to a periodic one to carry a rate, so intervals much
	// shorter than the typical one are skipped
	wr, rd := series{name: "write MiB/s"}, series{name: "read MiB/s"}
	var peakW, peakR float64
	minDT := medianInterval(samples) / 4
	for i := 1; i < len(samples); i++ {
		dt := samples[i].T.Sub(samples[i-1].T).Seconds()
		if dt <= 0 || dt < minDT {
			continue
		}
		w := float64(samples[i].S.BytesWritten-samples[i-1].S.BytesWritten) / dt / (1 << 20)
		r := float64(samples[i].S.BytesRead-samples[i-1].S.BytesRead) / dt / (1 << 20)
		wr.points = append(wr.points, [2]float64{x(&samples[i]), w})
		rd.points = append(rd.points, [2]float64{x(&samples[i]), r})
		peakW, peakR = maxf(peakW, w), maxf(peakR, r)
	}
	out = append(out, template.HTML(lineChart("Throughput (phase bands, event markers)", "MiB/s", []series{wr, rd}, marks, bands))) //nolint:gosec // generated SVG
	// repair backlog, storage, slots, liveness, WAL depth
	deg, pend, stale, unread := series{name: "degraded"}, series{name: "repair pending"}, series{name: "stale policy"}, series{name: "unreadable"}
	stored, logical := series{name: "stored bytes"}, series{name: "logical bytes"}
	wal, dirty := series{name: "overlay WAL bytes"}, series{name: "dirty bytes"}
	plan, flush := series{name: "plan bytes"}, series{name: "flush buffers"}
	heap, sys := series{name: "heap in use"}, series{name: "sys"}
	haveMem := false
	cpu, mcpu, load := series{name: "process % of one CPU"}, series{name: "machine % busy"}, series{name: "load (1 min)"}
	rss, avail := series{name: "resident set"}, series{name: "memory available"}
	pressure := series{name: "memory pressure (PSI some, avg10 %)"}
	free := series{name: "data disk free"}
	dwr, drd := series{name: "disk write MiB/s"}, series{name: "disk read MiB/s"}
	haveCPU, haveRSS, havePressure, haveDisk, haveDiskIO := false, false, false, false, false
	nodeSeries := map[string]*series{}
	var nodeNames []string
	liveCells, liveColors := map[string][][2]float64{}, map[string][]string{}
	var cells [][2]float64
	var colors []string
	haveWAL := false
	for i := range samples {
		rec := &samples[i]
		sm := rec.S
		deg.points = append(deg.points, [2]float64{x(rec), float64(sm.Degraded)})
		pend.points = append(pend.points, [2]float64{x(rec), float64(sm.RepairPending)})
		stale.points = append(stale.points, [2]float64{x(rec), float64(sm.StalePolicy)})
		unread.points = append(unread.points, [2]float64{x(rec), float64(sm.Unreadable)})
		stored.points = append(stored.points, [2]float64{x(rec), float64(sm.StoredBytes)})
		logical.points = append(logical.points, [2]float64{x(rec), float64(sm.LogicalBytes)})
		wal.points = append(wal.points, [2]float64{x(rec), float64(sm.WALBytes)})
		dirty.points = append(dirty.points, [2]float64{x(rec), float64(sm.DirtyBytes)})
		plan.points = append(plan.points, [2]float64{x(rec), float64(sm.PlanBytes)})
		flush.points = append(flush.points, [2]float64{x(rec), float64(sm.FlushBytes)})
		heap.points = append(heap.points, [2]float64{x(rec), float64(sm.HeapInuse)})
		sys.points = append(sys.points, [2]float64{x(rec), float64(sm.Sys)})
		haveWAL = haveWAL || sm.WALBytes > 0 || sm.DirtyBytes > 0
		haveMem = haveMem || sm.HeapInuse > 0
		cpu.points = append(cpu.points, [2]float64{x(rec), sm.CPUPct})
		mcpu.points = append(mcpu.points, [2]float64{x(rec), sm.MachinePct})
		load.points = append(load.points, [2]float64{x(rec), sm.Load1})
		rss.points = append(rss.points, [2]float64{x(rec), float64(sm.RSS)})
		avail.points = append(avail.points, [2]float64{x(rec), float64(sm.MemAvailable)})
		pressure.points = append(pressure.points, [2]float64{x(rec), sm.MemPressure})
		free.points = append(free.points, [2]float64{x(rec), float64(sm.DiskFree)})
		dwr.points = append(dwr.points, [2]float64{x(rec), sm.DiskWriteBps / (1 << 20)})
		drd.points = append(drd.points, [2]float64{x(rec), sm.DiskReadBps / (1 << 20)})
		haveCPU = haveCPU || sm.CPUPct > 0 || sm.MachinePct > 0
		haveRSS = haveRSS || sm.RSS > 0 || sm.MemAvailable > 0
		havePressure = havePressure || sm.MemPressure > 0
		haveDisk = haveDisk || sm.DiskFree > 0
		haveDiskIO = haveDiskIO || sm.DiskWriteBps > 0 || sm.DiskReadBps > 0
		for name, slots := range sm.Slots {
			ns, ok := nodeSeries[name]
			if !ok {
				ns = &series{name: name + " slots"}
				nodeSeries[name] = ns
				nodeNames = append(nodeNames, name)
			}
			ns.points = append(ns.points, [2]float64{x(rec), float64(slots)})
		}
		for name, live := range sm.LiveNodes {
			liveCells[name] = append(liveCells[name], [2]float64{x(rec), 0})
			c := "#16a34a"
			if !live {
				c = "#dc2626"
			}
			liveColors[name] = append(liveColors[name], c)
		}
		if sm.FSMAgree != nil {
			cells = append(cells, [2]float64{x(rec), 0})
			if *sm.FSMAgree {
				colors = append(colors, "#16a34a")
			} else {
				colors = append(colors, "#dc2626")
			}
		}
	}
	sort.Strings(nodeNames)
	maxX := s.Duration.Seconds()
	if len(samples) > 0 {
		maxX = maxf(maxX, x(&samples[len(samples)-1]))
	}
	out = append(out,
		template.HTML(lanes("Node liveness as seen by status (green = live)", maxX, nodeNames, liveCells, liveColors)), //nolint:gosec // generated SVG
		template.HTML(lineChart("Repair backlog", "chunks", []series{deg, pend, stale, unread}, marks, bands)),         //nolint:gosec // generated SVG
		template.HTML(lineChart("Stored vs logical bytes (dedup)", "bytes", []series{stored, logical}, nil, bands)))    //nolint:gosec // generated SVG
	if haveWAL {
		out = append(out, template.HTML(lineChart("Overlay WAL depth on the sampled node", "bytes", []series{wal, dirty, plan, flush}, marks, bands))) //nolint:gosec // generated SVG
	}
	if haveMem {
		out = append(out, template.HTML(lineChart("Process memory on the sampled node", "bytes", []series{heap, sys}, marks, bands))) //nolint:gosec // generated SVG
	}
	// machine vitals (docs/12 §8): each chart only when some node reported the measure
	if haveCPU {
		out = append(out, template.HTML(lineChart("CPU on the sampled node", "% / load", []series{cpu, mcpu, load}, marks, bands))) //nolint:gosec // generated SVG
	}
	if haveRSS {
		out = append(out, template.HTML(lineChart("Memory on the sampled node", "bytes", []series{rss, avail}, marks, bands))) //nolint:gosec // generated SVG
	}
	if havePressure {
		out = append(out, template.HTML(lineChart("Memory pressure on the sampled node", "%", []series{pressure}, marks, bands))) //nolint:gosec // generated SVG
	}
	if haveDisk {
		out = append(out, template.HTML(lineChart("Data disk on the sampled node", "bytes", []series{free}, marks, bands))) //nolint:gosec // generated SVG
	}
	if haveDiskIO {
		out = append(out, template.HTML(lineChart("Disk throughput on the sampled node (every disk)", "MiB/s", []series{dwr, drd}, marks, bands))) //nolint:gosec // generated SVG
	}
	if placement != nil {
		out = append(out, template.HTML(placementChart(placement))) //nolint:gosec // generated SVG
	}
	xs := make([]float64, 0, len(samples))
	spread := make([]*Sample, 0, len(samples))
	for i := range samples {
		xs = append(xs, x(&samples[i]))
		spread = append(spread, samples[i].S)
	}
	if svg := diskSpread(maxX, xs, spread); svg != "" {
		out = append(out, template.HTML(svg)) //nolint:gosec // generated SVG
	}
	ns := make([]series, 0, len(nodeNames))
	for _, n := range nodeNames {
		ns = append(ns, *nodeSeries[n])
	}
	out = append(out, template.HTML(lineChart("Shard slots per node", "slots", ns, marks, bands))) //nolint:gosec // generated SVG
	// latency histograms from the final counters
	if len(samples) > 0 {
		last := samples[len(samples)-1].S
		labels := make([]string, len(latencyBuckets))
		w, r := series{name: "writes"}, series{name: "reads"}
		for i := range latencyBuckets {
			if i == len(latencyBuckets)-1 {
				labels[i] = fmt.Sprintf(">%g", latencyBuckets[i-1])
			} else {
				labels[i] = fmt.Sprintf("≤%g", latencyBuckets[i])
			}
			w.points = append(w.points, [2]float64{float64(i), float64(last.WriteLatency[i])})
			r.points = append(r.points, [2]float64{float64(i), float64(last.ReadLatency[i])})
		}
		out = append(out, template.HTML(barChart("Operation latency, driver side (count per bucket, ms)", labels, []series{w, r}))) //nolint:gosec // generated SVG
	}
	out = append(out, template.HTML(strip("FSM-hash agreement across nodes (green = identical state at one index)", maxX, cells, colors))) //nolint:gosec // generated SVG
	return out, fmt.Sprintf("peak write %.1f MiB/s, peak read %.1f MiB/s", peakW, peakR)
}

// medianInterval is the median spacing of the samples in seconds (0 for fewer than two).
func medianInterval(samples []Record) float64 {
	var dts []float64
	for i := 1; i < len(samples); i++ {
		dts = append(dts, samples[i].T.Sub(samples[i-1].T).Seconds())
	}
	if len(dts) == 0 {
		return 0
	}
	sort.Float64s(dts)
	return dts[len(dts)/2]
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{"join": strings.Join}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>{{.Title}}</title>
<link rel="icon" href="data:image/svg+xml,{{.Favicon}}">
<style>
body{font:14px/1.45 -apple-system,Segoe UI,Helvetica,Arial,sans-serif;margin:0;padding:24px;color:#111;background:#fff;max-width:1000px}
h1{margin:0 0 4px;font-size:22px;display:flex;align-items:center;gap:10px;font-family:'JetBrains Mono','Fira Code',Consolas,monospace} h1 svg{width:28px;height:28px;color:#0f172a} h2{font-size:17px;margin:28px 0 8px;border-bottom:1px solid #ddd;padding-bottom:4px}
.banner{padding:14px 18px;border-radius:8px;font-size:20px;font-weight:600;color:#fff;margin:14px 0}
.pass{background:#16a34a}.fail{background:#dc2626}.partial{background:#d97706}
table{border-collapse:collapse;width:100%;font-size:13px} th,td{border:1px solid #e5e5e5;padding:5px 8px;text-align:left;vertical-align:top}
th{background:#f6f6f6} .ok{color:#16a34a;font-weight:600}.bad{color:#dc2626;font-weight:600}.skip{color:#777;font-weight:600}
.meta{color:#555} .cites{color:#777;font-size:11px;white-space:nowrap} .chart{width:100%;height:auto;margin:10px 0;border:1px solid #eee;border-radius:6px}
.title{font:600 13px sans-serif;fill:#222}.tick{font:11px sans-serif;fill:#555}.grid{stroke:#eee}.marker{stroke:#999;stroke-dasharray:3 3}.marklabel{font:9px sans-serif;fill:#777}
details summary{cursor:pointer} .checks td:first-child{width:38%} .num{text-align:right} .cov span{display:inline-block;margin:2px 6px 2px 0;padding:1px 6px;border-radius:4px;background:#eef2f7;font-size:12px} .cov .bad{background:#fee2e2} .dump{font:11px/1.35 ui-monospace,Menlo,Consolas,monospace;white-space:pre;overflow:auto;max-height:480px;background:#f8fafc;padding:8px;border:1px solid #eee}
</style></head><body>
<h1>{{.Mark}}<span>tail<span style="color:#2563eb">raid</span> soak report</span></h1>
<div class="meta">target: <b>{{.Env}}</b> · profile <b>{{.Profile}}</b> · policy {{.Policy}} · seed <code>{{.Seed}}</code> · flags {{.Flags}}<br>started {{.Started}} · duration {{.Duration}} · generated {{.Generated}}</div>
<div class="banner {{if eq .Verdict "PASS"}}pass{{else if eq .Verdict "FAIL"}}fail{{else}}partial{{end}}">{{.Verdict}} — {{.Checks}} checks, {{.Failed}} failed{{if .Skipped}}, skipped: {{join .Skipped ", "}}{{end}} · {{.Throughput}}</div>
<h2>Phases</h2>
<table><tr><th>Phase</th><th>Result</th><th>Checks</th><th>Took</th></tr>
{{range .Phases}}<tr><td>{{.Name}}</td><td class="{{if eq .Status "PASS"}}ok{{else if eq .Status "FAIL"}}bad{{else}}skip{{end}}">{{.Status}}{{if .Reason}} ({{.Reason}}){{end}}</td><td>{{len .Checks}}</td><td>{{.Took}}</td></tr>{{end}}
</table>
{{range .Phases}}<details{{if eq .Status "FAIL"}} open{{end}}><summary>{{.Name}} — {{.Status}}</summary>
<table class="checks">{{range .Checks}}<tr><td>{{.Name}}</td><td class="cites">{{.Cites}}</td><td class="{{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}ok{{else}}FAIL{{end}}</td><td>{{.Text}}</td></tr>{{end}}</table></details>{{end}}
<h2>Inconsistencies</h2>
{{if .Issues}}<table><tr><th>Phase</th><th>Check</th><th>Detail</th><th>File</th><th>Chunk</th><th>Node</th><th>Record</th></tr>
{{range .Issues}}<tr><td>{{.Phase}}</td><td>{{.Name}}</td><td>{{.Text}}</td><td>{{.File}}</td><td>{{.Chunk}}</td><td>{{.Node}}</td><td><code>{{.Where}}</code></td></tr>{{end}}</table>
{{else}}<p class="ok">none</p>{{end}}
{{if .Dumps}}<h2>Goroutine dumps</h2>
<p class="meta">every node's goroutines, taken right after a failed phase (dev.enabled); a hang shows as goroutines blocked for minutes</p>
{{range .Dumps}}<details><summary>{{.Phase}} — {{.Node}} ({{.Lines}} lines)</summary><pre class="dump">{{.Text}}</pre></details>{{end}}{{end}}
<h2>Totals</h2>
<table>{{range .Totals}}<tr><th>{{.K}}</th><td>{{.V}}</td></tr>{{end}}</table>
<h2>Latency (driver side)</h2>
<table><tr><th>Phase</th><th>Op</th><th>Window</th><th class="num">n</th><th class="num">p50</th><th class="num">p99</th><th class="num">max</th></tr>
{{range .Latency}}<tr><td>{{.Phase}}</td><td>{{.Kind}}</td><td>{{.Window}}</td><td class="num">{{.N}}</td><td class="num">{{.P50}}</td><td class="num">{{.P99}}</td><td class="num">{{.Max}}</td></tr>{{end}}</table>
<h2>Coverage</h2>
<p class="meta">invariants (I-n) and lifecycle cells (L-n) cited by the checks that ran, with pass/fail counts (docs/invariants.md, docs/lifecycle.md)</p>
<div class="cov">{{range .Coverage}}<span{{if .Failed}} class="bad"{{end}}>{{.ID}} {{.Passed}}{{if .Failed}}/<b>{{.Failed}} failed</b>{{end}}</span>{{end}}</div>
<h2>Notes</h2>
{{if .Notes}}<table><tr><th>Phase</th><th>Note</th></tr>{{range .Notes}}<tr><td>{{.Phase}}</td><td>{{.Text}}</td></tr>{{end}}</table>{{else}}<p class="meta">none</p>{{end}}
<h2>Charts</h2>
{{range .Charts}}{{.}}{{end}}
{{if .Disks}}<h2>Disks</h2>
<p class="meta">every placement target as of the last sample that held data, grouped by host (docs/14); expected is the capacity share capped at one slot per chunk; most chunks above the host cap in any sample: {{.PeakSkew}}</p>
<table><tr><th>Host</th><th>Target</th><th class="num">Budget</th><th class="num">Slots</th><th class="num">Expected</th><th class="num">Δ</th><th>State</th></tr>
{{range .Disks}}<tr><td>{{.Host}}</td><td>{{.Name}}</td><td class="num">{{.Budget}}</td><td class="num">{{.Slots}}</td><td class="num">{{.Expected}}</td><td class="num">{{.Delta}}</td><td>{{.State}}</td></tr>{{end}}</table>{{end}}
<h2>Nodes</h2>
<table><tr><th>Name</th><th>Idx</th><th>RPC</th><th>Version</th></tr>{{range .Nodes}}<tr><td>{{.Name}}</td><td>{{.Idx}}</td><td>{{.RPC}}</td><td>{{.Version}}</td></tr>{{end}}</table>
</body></html>
`))
