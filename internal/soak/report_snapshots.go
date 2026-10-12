package soak

import (
	"fmt"
	"html/template"
	"slices"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/chart"
)

// The snapshot figures of phase 19 (docs/09 §6, the lookdev
// assets/lookdev/soak-snapshots.html): each mirror's bytes over the run with the
// snapshots marked and the unplugged stretch as a band, what each snapshot added to its
// mirror on a log axis, and the round-trip table. Drawn only when a snapshot was recorded.

// snapView is one row of the round-trip table.
type snapView struct {
	Name, Mirror, Taken, Files, Bytes, Added, State, Read string
}

// snapshotFigures draws the two charts and builds the table from the run's records.
func snapshotFigures(s Summary) (charts []template.HTML, rows []snapView, roundTrip string) {
	var snaps []*Record
	var offAt, onAt float64 = -1, -1
	used := map[string]*chart.Series{}
	x := func(rec *Record) float64 { return rec.T.Sub(s.Started).Seconds() }
	for i := range s.Records {
		rec := &s.Records[i]
		switch {
		case rec.Kind == "sample" && rec.S != nil:
			for name, b := range rec.S.MirrorUsed {
				sr := used[name]
				if sr == nil {
					sr = &chart.Series{Name: name + " used"}
					used[name] = sr
				}
				sr.Points = append(sr.Points, [2]float64{x(rec), float64(b)})
			}
		case rec.Kind != "event":
		case rec.Name == snapshotEvent:
			snaps = append(snaps, rec)
		case rec.Name == mirrorOffline:
			offAt = x(rec)
		case rec.Name == mirrorOnline:
			onAt = x(rec)
		case rec.Name == roundTripEvent:
			roundTrip = rec.Text
		}
	}
	if len(snaps) == 0 {
		return nil, nil, ""
	}
	// a snapshot's instant: its catalogue time where that falls inside the run (a real
	// clock), else when the phase recorded it (the harness's fake clock)
	at := func(rec *Record) float64 {
		if ns, err := strconv.ParseInt(rec.Attrs["at"], 10, 64); err == nil {
			if t := float64(ns-s.Started.UnixNano()) / 1e9; t >= 0 && t <= s.Duration.Seconds() {
				return t
			}
		}
		return x(rec)
	}
	var marks []chart.Marker
	var bars []chart.Bar
	for _, rec := range snaps {
		a := rec.Attrs
		label := a["label"]
		switch label {
		case "automatic", "manual":
		case "first":
			marks = append(marks, chart.Marker{X: at(rec), Label: "first sync"})
		default:
			marks = append(marks, chart.Marker{X: at(rec), Label: label})
		}
		nb, _ := strconv.ParseFloat(a["new_bytes"], 64)
		total, _ := strconv.ParseFloat(a["bytes"], 64)
		cls := ""
		if label == "first" {
			cls = "chart-off"
		}
		bars = append(bars, chart.Bar{Label: label, Value: nb, Class: cls, Tip: fmt.Sprintf("%s (%s): %s of %s", rec.Text, label, humanBytes(nb), humanBytes(total))})
		taken := fmt.Sprintf("%.0f s · index %s", at(rec), a["index"])
		if a["auto"] == "true" {
			taken += ", automatic"
		}
		rows = append(rows, snapView{Name: rec.Text, Mirror: a["mirror"], Taken: taken, Files: a["files"], Bytes: humanBytes(total), Added: humanBytes(nb), State: a["state"], Read: a["read"]})
	}
	var bands []chart.Band
	if offAt >= 0 {
		end := onAt
		if end < offAt {
			end = s.Duration.Seconds()
		}
		bands = append(bands, chart.Band{X0: offAt, X1: end, Label: "offline"})
	}
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	slices.Sort(names)
	series := make([]chart.Series, 0, len(names))
	for _, n := range names {
		series = append(series, *used[n])
	}
	mirrors := strings.Join(names, ", ")
	charts = append(charts,
		template.HTML(chart.Line("Mirror "+mirrors+" — bytes on the disk (mirrors[].used)", "bytes", series, marks, bands, chart.Axis{})), //nolint:gosec // generated SVG
		template.HTML(chart.LogBars("What each snapshot added to its mirror (new_bytes), log scale; grey: the first sync copies the volume", bars, func(v float64) string { //nolint:gosec // generated SVG
			if v < 1 {
				return "0"
			}
			return humanBytes(v)
		})))
	return charts, rows, roundTrip
}
