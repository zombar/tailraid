package soak

import (
	"math"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/chart"
)

// The expected share is the capacity share capped at one slot per chunk (I-9): with as
// many targets as a stripe has slots, every target holds every chunk whatever its size.
// Invariant: I-87
func TestReport_ExpectedShares_CappedPerChunk(t *testing.T) {
	ts := []TargetSample{
		{Name: "a", Slots: 20, Capacity: 2, Active: true, Live: true},
		{Name: "a/1", Slots: 20, Capacity: 4, Active: true, Live: true},
		{Name: "a/2", Slots: 20, Capacity: 4, Active: true, Live: true},
		{Name: "a/3", Slots: 20, Capacity: 2, Active: true, Live: true},
	}
	for i, e := range chart.ExpectedShares(ts, 20) {
		if math.Abs(e-20) > 1e-9 {
			t.Errorf("target %d expects %.2f, want 20 (every chunk)", i, e)
		}
	}
	// without the cap binding it is the plain capacity share; a draining target expects 0
	ts = []TargetSample{
		{Name: "a", Slots: 10, Capacity: 1, Active: true},
		{Name: "b", Slots: 20, Capacity: 3, Active: true},
		{Name: "c", Slots: 5, Capacity: 3, Active: false},
	}
	e := chart.ExpectedShares(ts, 1000)
	if math.Abs(e[0]-8.75) > 1e-9 || math.Abs(e[1]-26.25) > 1e-9 || e[2] != 0 {
		t.Fatalf("shares %v", e)
	}
}

// The Disks table reads the last sample that still held data and the peak skew of all.
// Invariant: I-89
func TestReport_DisksTable_LastSampleWithData(t *testing.T) {
	held := &Sample{DomainSkew: 3, Chunks: 10, Targets: []TargetSample{{Name: "a", Host: "a", Slots: 10, Capacity: 1, Active: true, Live: true}, {Name: "a/1", Host: "a", Slots: 10, Capacity: 1, Active: true, Live: false}}}
	empty := &Sample{Targets: []TargetSample{{Name: "a", Host: "a", Capacity: 1, Active: true, Live: true}}}
	rows, peak := disksTable([]*Sample{held, empty})
	if peak != 3 || len(rows) != 2 || rows[0].Slots != 10 || rows[1].State != "dead" || rows[0].Delta != "+0.0 %" {
		t.Fatalf("rows %+v peak %d", rows, peak)
	}
	svg := chart.Spread(10, []float64{0, 5}, spreadSamples([]*Sample{held, empty}), chart.Axis{})
	if !strings.Contains(svg, "Data spread across disks") || !strings.Contains(svg, `fill="url(#dead)"`) || !strings.Contains(svg, `class="chart-warn"`) {
		t.Fatal("chart lacks its lanes, the dead hatch or the skew strip")
	}
	if chart.Spread(10, []float64{0}, spreadSamples([]*Sample{{}}), chart.Axis{}) != "" {
		t.Fatal("a run without disks drew the chart")
	}
}
