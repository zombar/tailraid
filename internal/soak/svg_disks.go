package soak

import (
	"fmt"
	"html"
	"math"
	"slices"
	"strings"
)

// The report's view of several disks per device (docs/14 §8, docs/09 §6): one lane per
// target over time, each sample coloured by the target's slot share against its expected
// share, with a host-cap strip underneath. The colour scale is the signed-off mock in
// docs/assets/lookdev/soak-disk-spread.html.

// spreadBins are the deviation bins and their colours: blue under, grey at the expected
// share, violet over (status colours stay for state: dead is hatched red).
var spreadBins = []struct {
	hi    float64
	color string
	label string
}{
	{-0.30, "#1d4ed8", "< −30 %"},
	{-0.15, "#7895e4", "−30…−15 %"},
	{-0.05, "#c3cdee", "−15…−5 %"},
	{0.05, "#e5e7eb", "±5 %"},
	{0.15, "#e3c3e6", "+5…+15 %"},
	{0.30, "#c272c9", "+15…+30 %"},
	{math.Inf(1), "#a21caf", "> +30 %"},
}

func spreadColor(dev float64) string {
	for _, b := range spreadBins {
		if dev < b.hi {
			return b.color
		}
	}
	return spreadBins[len(spreadBins)-1].color
}

// expectedShares spreads the slots of a sample over its active targets by capacity, no
// target expecting more than one slot per chunk (I-9); the rest goes to the others.
func expectedShares(ts []TargetSample, chunks int) []float64 {
	total := 0.0
	for _, t := range ts {
		total += float64(t.Slots)
	}
	out := make([]float64, len(ts))
	open := map[int]bool{}
	for i, t := range ts {
		if t.Active && t.Capacity > 0 {
			open[i] = true
		}
	}
	left := total
	limit := float64(chunks)
	if chunks <= 0 {
		limit = math.Inf(1)
	}
	for len(open) > 0 {
		sumCap := 0.0
		for i := range open {
			sumCap += float64(ts[i].Capacity)
		}
		capped := false
		for i := range open {
			if left*float64(ts[i].Capacity)/sumCap > limit {
				out[i] = limit
				left -= limit
				delete(open, i)
				capped = true
			}
		}
		if !capped {
			for i := range open {
				out[i] = left * float64(ts[i].Capacity) / sumCap
			}
			break
		}
	}
	return out
}

// diskSpread draws the lanes. xs are the samples' x positions; maxX the run's length.
func diskSpread(maxX float64, xs []float64, samples []*Sample) string {
	// lanes from the last sample that has targets, grouped by host
	var lanes []TargetSample
	for _, sample := range slices.Backward(samples) {
		if len(sample.Targets) > 0 {
			lanes = sample.Targets
			break
		}
	}
	if len(lanes) == 0 {
		return ""
	}
	const rowH, gap = 14, 6
	iw := float64(chartW - padL - padR)
	if maxX == 0 {
		maxX = 1
	}
	laneY := map[string]int{}
	y := padT
	host := ""
	for _, t := range lanes {
		if host != "" && t.Host != host {
			y += gap
		}
		host = t.Host
		laneY[t.Name] = y
		y += rowH
	}
	capY := y + 6
	h := capY + rowH + 64
	var b strings.Builder
	title := "Data spread across disks — each target's slot share against its expected share"
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`, chartW, h, html.EscapeString(title))
	b.WriteString(`<defs><pattern id="dead" width="4" height="4" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="4" height="4" fill="#fff"/><rect width="1.6" height="4" fill="#dc2626"/></pattern></defs>`)
	fmt.Fprintf(&b, `<text x="%d" y="18" class="title">%s</text>`, padL, html.EscapeString(title))
	for _, t := range lanes {
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="tick" text-anchor="end">%s</text>`, padL-6, laneY[t.Name]+11, html.EscapeString(t.Name))
	}
	// cells: each sample spans to the next one
	for i, s := range samples {
		if len(s.Targets) == 0 {
			continue
		}
		x0 := float64(padL) + xs[i]/maxX*iw
		x1 := x0 + 4
		if i+1 < len(samples) {
			x1 = math.Max(x0+1, float64(padL)+xs[i+1]/maxX*iw)
		}
		exp := expectedShares(s.Targets, s.Chunks)
		for j, t := range s.Targets {
			ly, ok := laneY[t.Name]
			if !ok || j >= len(exp) {
				continue
			}
			fill, tip := "#e5e7eb", "draining"
			switch {
			case !t.Live:
				fill, tip = "url(#dead)", "dead"
			case t.Active && exp[j] > 0:
				dev := (float64(t.Slots) - exp[j]) / exp[j]
				fill, tip = spreadColor(dev), fmt.Sprintf("%+.0f %% of expected (%d of %.0f slots)", dev*100, t.Slots, exp[j])
			case t.Active:
				tip = "no budget"
			}
			fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.2f" height="%d" fill="%s"><title>%s at %.0fs: %s</title></rect>`, x0, ly+1, x1-x0, rowH-2, fill, html.EscapeString(t.Name), xs[i], html.EscapeString(tip)) //nolint:gosec // xs has one entry per sample
		}
		c, tip := "#16a34a", "no chunk above the host cap"
		if s.DomainSkew > 0 {
			c, tip = "#d97706", fmt.Sprintf("%d chunks above the host cap", s.DomainSkew)
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.2f" height="%d" fill="%s"><title>%.0fs: %s</title></rect>`, x0, capY+1, x1-x0, rowH-2, c, xs[i], tip) //nolint:gosec // as above
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="tick" text-anchor="end">host cap</text>`, padL-6, capY+11)
	ay := capY + rowH + 14
	for i := range 7 {
		x := float64(padL) + iw*float64(i)/6
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%ss</text>`, x, ay, fmtNum(maxX*float64(i)/6))
	}
	lx, ly := padL, ay+18
	for _, bin := range spreadBins {
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" fill="%s"/><text x="%d" y="%d" class="tick">%s</text>`, lx, ly-9, bin.color, lx+16, ly, html.EscapeString(bin.label))
		lx += 16 + len([]rune(bin.label))*6 + 14
	}
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" fill="url(#dead)"/><text x="%d" y="%d" class="tick">dead</text>`, lx, ly-9, lx+16, ly)
	ly += 16
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" fill="#16a34a"/><text x="%d" y="%d" class="tick">within host cap</text>`, padL, ly-9, padL+16, ly)
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" fill="#d97706"/><text x="%d" y="%d" class="tick">chunks above host cap (domain_skew &gt; 0)</text>`, padL+130, ly-9, padL+146, ly)
	b.WriteString("</svg>")
	return b.String()
}

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
	exp := expectedShares(last.Targets, last.Chunks)
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
