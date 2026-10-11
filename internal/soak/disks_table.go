package soak

import (
	"fmt"

	"github.com/zombar/tailraid/internal/chart"
)

// The report's Disks table (docs/14 §8, docs/09 §6): every target at the end of the run
// with its expected share; the lanes over time are chart.Spread.

// diskView is one row of the report's Disks table: a target at the end of the run.
type diskView struct {
	Host, Name, State string
	Budget            string
	Slots             int
	Expected          string
	Delta             string
}

// disksTable lists the targets of the last sample that still held data (the run removes
// its corpus at the end) with their expected share.
func disksTable(samples []*Sample) (rows []diskView, peakSkew int) {
	var last *Sample
	for _, s := range samples {
		peakSkew = max(peakSkew, s.DomainSkew)
		held := 0
		for _, t := range s.Targets {
			held += t.Slots
		}
		if held > 0 {
			last = s
		}
	}
	if last == nil {
		return nil, 0
	}
	exp := chart.ExpectedShares(last.Targets, last.Chunks)
	for i, t := range last.Targets {
		state := "live"
		switch {
		case !t.Live:
			state = "dead"
		case !t.Active:
			state = "draining"
		}
		row := diskView{Host: t.Host, Name: t.Name, State: state, Budget: humanBytes(float64(t.Capacity)), Slots: t.Slots, Expected: "—", Delta: "—"}
		if exp[i] > 0 {
			row.Expected = fmt.Sprintf("%.0f", exp[i])
			row.Delta = fmt.Sprintf("%+.1f %%", (float64(t.Slots)-exp[i])/exp[i]*100)
		}
		rows = append(rows, row)
	}
	return rows, peakSkew
}
