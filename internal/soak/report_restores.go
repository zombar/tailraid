package soak

import (
	"html/template"
	"slices"
	"strconv"

	"github.com/zombar/tailraid/internal/chart"
)

// The restore figures of phase 32 (docs/09 §6, docs/16 §6): each job's bytes done over the
// run with what was done to the cluster marked (the mirror unplugged and back, the host
// restarted, a cancel), and a table of the jobs. Drawn only when a job was followed.

// restoreView is one row of the jobs table.
type restoreView struct {
	ID, Label, Dst, State, Files, Bytes, Existing, Resumed, Took string
}

// restoreFigures draws the chart and builds the table from the run's records.
func restoreFigures(s Summary) (charts []template.HTML, rows []restoreView) {
	series := map[string]*chart.Series{}
	var order []string
	var marks []chart.Marker
	x := func(rec *Record) float64 { return rec.T.Sub(s.Started).Seconds() }
	for i := range s.Records {
		rec := &s.Records[i]
		if rec.Kind != "event" {
			continue
		}
		switch rec.Name {
		case restoreSample:
			sr := series[rec.Text]
			if sr == nil {
				name := rec.Text
				if l := rec.Attrs["label"]; l != "" {
					name += " (" + l + ")"
				}
				sr = &chart.Series{Name: name}
				series[rec.Text] = sr
				order = append(order, rec.Text)
			}
			b, _ := strconv.ParseFloat(rec.Attrs["bytes"], 64)
			sr.Points = append(sr.Points, [2]float64{x(rec), b})
		case restoreMark:
			marks = append(marks, chart.Marker{X: x(rec), Label: rec.Text})
		case restoreRow:
			a := rec.Attrs
			rows = append(rows, restoreView{ID: rec.Text, Label: a["label"], Dst: a["dst"], State: a["state"], Files: a["files"], Bytes: a["bytes"], Existing: a["existing"], Resumed: a["resumed"], Took: a["took"]})
		}
	}
	if len(order) == 0 {
		return nil, rows
	}
	slices.Sort(order)
	lines := make([]chart.Series, 0, len(order))
	for _, id := range order {
		lines = append(lines, *series[id])
	}
	charts = append(charts, template.HTML(chart.Line("Restore jobs — bytes done (status.restores[].bytes_done)", "bytes", lines, marks, nil, chart.Axis{}))) //nolint:gosec // generated SVG
	return charts, rows
}
