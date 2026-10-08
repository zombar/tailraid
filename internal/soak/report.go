package soak

import (
	"fmt"
	"html/template"
	"io"
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
	Started    string
	Duration   string
	Pass       bool
	Failed     int
	Checks     int
	Skipped    int
	Nodes      []NodeRef
	Phases     []phaseView
	Issues     []Record
	Charts     []template.HTML
	Generated  string
	Throughput string
}

type phaseView struct {
	Name   string
	Status string
	Took   string
	Checks []checkView
	Reason string
}

type checkView struct {
	Name string
	OK   bool
	Text string
}

func buildReport(s Summary) reportData {
	d := reportData{Title: "tailraid soak report", Env: s.Env, Profile: s.Profile, Started: s.Started.UTC().Format(time.RFC3339),
		Duration: s.Duration.Round(time.Second).String(), Nodes: s.Nodes, Generated: time.Now().UTC().Format(time.RFC3339), Failed: s.Failed}
	for _, ph := range s.Phases {
		pv := phaseView{Name: ph.Name, Took: ph.Took.Round(time.Millisecond).String(), Reason: ph.Reason}
		switch {
		case ph.Skipped:
			pv.Status = "SKIPPED"
			d.Skipped++
		case ph.Failed > 0:
			pv.Status = "FAIL"
		default:
			pv.Status = "PASS"
		}
		for i := range ph.Checks {
			c := &ph.Checks[i]
			ok := c.OK == nil || *c.OK
			pv.Checks = append(pv.Checks, checkView{Name: c.Name, OK: ok, Text: c.Text})
			d.Checks++
			if !ok {
				d.Issues = append(d.Issues, *c)
			}
		}
		d.Phases = append(d.Phases, pv)
	}
	d.Pass = d.Failed == 0
	d.Charts, d.Throughput = charts(s)
	return d
}

// charts derives every figure from the sample and event records.
func charts(s Summary) ([]template.HTML, string) {
	var samples []Record
	var marks []marker
	for i := range s.Records {
		rec := &s.Records[i]
		switch rec.Kind {
		case "sample":
			if rec.S != nil {
				samples = append(samples, *rec)
			}
		case "event":
			if rec.Name != "note" && rec.Name != "baseline written" {
				marks = append(marks, marker{x: rec.T.Sub(s.Started).Seconds(), label: rec.Name + " " + rec.Text})
			}
		}
	}
	if len(marks) > 24 { // keep the labels readable
		marks = marks[:24]
	}
	x := func(rec *Record) float64 { return rec.T.Sub(s.Started).Seconds() }
	var out []template.HTML
	// throughput: bytes per second between consecutive samples
	wr, rd := series{name: "write MiB/s"}, series{name: "read MiB/s"}
	var peakW, peakR float64
	for i := 1; i < len(samples); i++ {
		dt := samples[i].T.Sub(samples[i-1].T).Seconds()
		if dt <= 0 {
			continue
		}
		w := float64(samples[i].S.BytesWritten-samples[i-1].S.BytesWritten) / dt / (1 << 20)
		r := float64(samples[i].S.BytesRead-samples[i-1].S.BytesRead) / dt / (1 << 20)
		wr.points = append(wr.points, [2]float64{x(&samples[i]), w})
		rd.points = append(rd.points, [2]float64{x(&samples[i]), r})
		peakW, peakR = maxf(peakW, w), maxf(peakR, r)
	}
	out = append(out, template.HTML(lineChart("Throughput with event markers", "MiB/s", []series{wr, rd}, marks))) //nolint:gosec // generated SVG
	// repair backlog
	deg, stale, unread := series{name: "degraded chunks"}, series{name: "stale-policy chunks"}, series{name: "unreadable chunks"}
	stored, logical := series{name: "stored bytes"}, series{name: "logical bytes"}
	nodeSeries := map[string]*series{}
	var nodeNames []string
	var cells [][2]float64
	var colors []string
	for i := range samples {
		rec := &samples[i]
		sm := rec.S
		deg.points = append(deg.points, [2]float64{x(rec), float64(sm.Degraded)})
		stale.points = append(stale.points, [2]float64{x(rec), float64(sm.StalePolicy)})
		unread.points = append(unread.points, [2]float64{x(rec), float64(sm.Unreadable)})
		stored.points = append(stored.points, [2]float64{x(rec), float64(sm.StoredBytes)})
		logical.points = append(logical.points, [2]float64{x(rec), float64(sm.LogicalBytes)})
		for name, slots := range sm.Slots {
			ns, ok := nodeSeries[name]
			if !ok {
				ns = &series{name: name + " slots"}
				nodeSeries[name] = ns
				nodeNames = append(nodeNames, name)
			}
			ns.points = append(ns.points, [2]float64{x(rec), float64(slots)})
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
	out = append(out,
		template.HTML(lineChart("Repair backlog", "chunks", []series{deg, stale, unread}, marks)),            //nolint:gosec // generated SVG
		template.HTML(lineChart("Stored vs logical bytes (dedup)", "bytes", []series{stored, logical}, nil))) //nolint:gosec // generated SVG
	sort.Strings(nodeNames)
	ns := make([]series, 0, len(nodeNames))
	for _, n := range nodeNames {
		ns = append(ns, *nodeSeries[n])
	}
	out = append(out, template.HTML(lineChart("Shard slots per node", "slots", ns, marks))) //nolint:gosec // generated SVG
	// latency histograms from the final counters
	if len(samples) > 0 {
		last := samples[len(samples)-1].S
		labels := make([]string, len(latencyBuckets))
		w, r := series{name: "writes"}, series{name: "reads"}
		for i := range latencyBuckets {
			if i == len(latencyBuckets)-1 {
				labels[i] = fmt.Sprintf(">%gms", latencyBuckets[i-1])
			} else {
				labels[i] = fmt.Sprintf("≤%gms", latencyBuckets[i])
			}
			w.points = append(w.points, [2]float64{float64(i), float64(last.WriteLatency[i])})
			r.points = append(r.points, [2]float64{float64(i), float64(last.ReadLatency[i])})
		}
		out = append(out, template.HTML(barChart("Operation latency (driver-side, count per bucket)", labels, []series{w, r}))) //nolint:gosec // generated SVG
	}
	maxX := 0.0
	if len(samples) > 0 {
		maxX = x(&samples[len(samples)-1])
	}
	out = append(out, template.HTML(strip("FSM-hash agreement across nodes (green = identical state at one index)", maxX, cells, colors))) //nolint:gosec // generated SVG
	return out, fmt.Sprintf("peak write %.1f MiB/s, peak read %.1f MiB/s", peakW, peakR)
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{"join": strings.Join}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>{{.Title}}</title>
<style>
body{font:14px/1.45 -apple-system,Segoe UI,Helvetica,Arial,sans-serif;margin:0;padding:24px;color:#111;background:#fff;max-width:1000px}
h1{margin:0 0 4px;font-size:22px} h2{font-size:17px;margin:28px 0 8px;border-bottom:1px solid #ddd;padding-bottom:4px}
.banner{padding:14px 18px;border-radius:8px;font-size:20px;font-weight:600;color:#fff;margin:14px 0}
.pass{background:#16a34a}.fail{background:#dc2626}
table{border-collapse:collapse;width:100%;font-size:13px} th,td{border:1px solid #e5e5e5;padding:5px 8px;text-align:left;vertical-align:top}
th{background:#f6f6f6} .ok{color:#16a34a;font-weight:600}.bad{color:#dc2626;font-weight:600}.skip{color:#777;font-weight:600}
.meta{color:#555} .chart{width:100%;height:auto;margin:10px 0;border:1px solid #eee;border-radius:6px}
.title{font:600 13px sans-serif;fill:#222}.tick{font:11px sans-serif;fill:#555}.grid{stroke:#eee}.marker{stroke:#999;stroke-dasharray:3 3}.marklabel{font:9px sans-serif;fill:#777}
details summary{cursor:pointer} .checks td:first-child{width:38%}
</style></head><body>
<h1>{{.Title}}</h1>
<div class="meta">target: <b>{{.Env}}</b> · profile <b>{{.Profile}}</b> · started {{.Started}} · duration {{.Duration}} · generated {{.Generated}}</div>
<div class="banner {{if .Pass}}pass{{else}}fail{{end}}">{{if .Pass}}PASS{{else}}FAIL{{end}} — {{.Checks}} checks, {{.Failed}} failed, {{.Skipped}} phases skipped · {{.Throughput}}</div>
<h2>Phases</h2>
<table><tr><th>Phase</th><th>Result</th><th>Checks</th><th>Took</th></tr>
{{range .Phases}}<tr><td>{{.Name}}</td><td class="{{if eq .Status "PASS"}}ok{{else if eq .Status "FAIL"}}bad{{else}}skip{{end}}">{{.Status}}{{if .Reason}} ({{.Reason}}){{end}}</td><td>{{len .Checks}}</td><td>{{.Took}}</td></tr>{{end}}
</table>
{{range .Phases}}<details{{if eq .Status "FAIL"}} open{{end}}><summary>{{.Name}} — {{.Status}}</summary>
<table class="checks">{{range .Checks}}<tr><td>{{.Name}}</td><td class="{{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}ok{{else}}FAIL{{end}}</td><td>{{.Text}}</td></tr>{{end}}</table></details>{{end}}
<h2>Inconsistencies</h2>
{{if .Issues}}<table><tr><th>Phase</th><th>Check</th><th>Detail</th><th>File</th><th>Chunk</th><th>Node</th></tr>
{{range .Issues}}<tr><td>{{.Phase}}</td><td>{{.Name}}</td><td>{{.Text}}</td><td>{{.File}}</td><td>{{.Chunk}}</td><td>{{.Node}}</td></tr>{{end}}</table>
{{else}}<p class="ok">none</p>{{end}}
<h2>Charts</h2>
{{range .Charts}}{{.}}{{end}}
<h2>Nodes</h2>
<table><tr><th>Name</th><th>Idx</th><th>RPC</th></tr>{{range .Nodes}}<tr><td>{{.Name}}</td><td>{{.Idx}}</td><td>{{.RPC}}</td></tr>{{end}}</table>
</body></html>
`))
