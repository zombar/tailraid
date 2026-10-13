package chart

import (
	"fmt"
	"html"
	"math"
	"slices"
	"strings"
)

// The disk-spread lanes of docs/14 §8 and docs/09 §6: one lane per target over time,
// each sample coloured by the target's slot share against its expected share, with a
// host-cap strip underneath. The bins are the signed-off mock in
// docs/assets/lookdev/soak-disk-spread.html; the classes carry no colour of their own.

// Target is one placement target in a sample (the soak's TargetSample is this type).
type Target struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Slots    int    `json:"slots"`
	Capacity uint64 `json:"capacity"`
	Live     bool   `json:"live"`
	Active   bool   `json:"active"`
}

// SpreadSample is one sample of the lanes: the targets, the live chunk count (the most
// slots one target can hold, I-9) and the chunks above the host cap.
type SpreadSample struct {
	Targets    []Target
	Chunks     int
	DomainSkew int
}

// spreadBins are the deviation bins: blue under the expected share, grey at it, violet
// over (status classes stay for state: dead is hatched).
var spreadBins = []struct {
	hi    float64
	class string
	label string
}{
	{-0.30, "dev-3", "< −30 %"},
	{-0.15, "dev-2", "−30…−15 %"},
	{-0.05, "dev-1", "−15…−5 %"},
	{0.05, "dev0", "±5 %"},
	{0.15, "dev1", "+5…+15 %"},
	{0.30, "dev2", "+15…+30 %"},
	{math.Inf(1), "dev3", "> +30 %"},
}

func spreadClass(dev float64) string {
	for _, b := range spreadBins {
		if dev < b.hi {
			return b.class
		}
	}
	return spreadBins[len(spreadBins)-1].class
}

// ExpectedShares spreads the slots of a sample over its active targets by capacity, no
// target expecting more than one slot per chunk (I-9); the rest goes to the others.
func ExpectedShares(ts []Target, chunks int) []float64 {
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

// The spread's lane height and the gap between hosts.
const spreadRow, spreadGap = 14, 6

// SpreadHeight is the height of a Spread chart whose lanes are targets on these hosts, in
// lane order (a host's targets together).
func SpreadHeight(hosts []string) int {
	y := padT
	for i, h := range hosts {
		if i > 0 && h != hosts[i-1] {
			y += spreadGap
		}
		y += spreadRow
	}
	return y + 6 + spreadRow + 64
}

// Spread draws the lanes. xs are the samples' x positions; maxX the span.
func Spread(maxX float64, xs []float64, samples []SpreadSample, ax Axis) string {
	// lanes from the last sample that has targets, grouped by host
	var lanes []Target
	for _, sample := range slices.Backward(samples) {
		if len(sample.Targets) > 0 {
			lanes = sample.Targets
			break
		}
	}
	if len(lanes) == 0 {
		return ""
	}
	const rowH, gap = spreadRow, spreadGap
	iw := float64(Width - padL - padR)
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
	h := capY + rowH + 64 // SpreadHeight
	var b strings.Builder
	title := "Data spread across disks — each target's slot share against its expected share"
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`, Width, h, html.EscapeString(title))
	b.WriteString(`<defs><pattern id="dead" width="4" height="4" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="4" height="4" class="chart-bg"/><rect width="1.6" height="4" class="chart-bad"/></pattern></defs>`)
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
		exp := ExpectedShares(s.Targets, s.Chunks)
		for j, t := range s.Targets {
			ly, ok := laneY[t.Name]
			if !ok || j >= len(exp) {
				continue
			}
			fill, tip := `class="dev0"`, "draining"
			switch {
			case !t.Live:
				fill, tip = `fill="url(#dead)"`, "dead"
			case t.Active && exp[j] > 0:
				dev := (float64(t.Slots) - exp[j]) / exp[j]
				fill, tip = `class="`+spreadClass(dev)+`"`, fmt.Sprintf("%+.0f %% of expected (%d of %.0f slots)", dev*100, t.Slots, exp[j])
			case t.Active:
				tip = "no budget"
			}
			fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.2f" height="%d" %s><title>%s at %s: %s</title></rect>`, x0, ly+1, x1-x0, rowH-2, fill, html.EscapeString(t.Name), ax.at(xs[i], maxX), html.EscapeString(tip)) //nolint:gosec // xs has one entry per sample
		}
		c, tip := OK, "no chunk above the host cap"
		if s.DomainSkew > 0 {
			c, tip = Warn, fmt.Sprintf("%d chunks above the host cap", s.DomainSkew)
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.2f" height="%d" class="%s"><title>%s: %s</title></rect>`, x0, capY+1, x1-x0, rowH-2, c, ax.at(xs[i], maxX), tip) //nolint:gosec // as above
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="tick" text-anchor="end">host cap</text>`, padL-6, capY+11)
	ay := capY + rowH + 14
	xTicks(&b, ay, maxX, ax)
	lx, ly := padL, ay+18
	for _, bin := range spreadBins {
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" class="%s"/><text x="%d" y="%d" class="tick">%s</text>`, lx, ly-9, bin.class, lx+16, ly, html.EscapeString(bin.label))
		lx += 16 + len([]rune(bin.label))*6 + 14
	}
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" fill="url(#dead)"/><text x="%d" y="%d" class="tick">dead</text>`, lx, ly-9, lx+16, ly)
	ly += 16
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" class="%s"/><text x="%d" y="%d" class="tick">within host cap</text>`, padL, ly-9, OK, padL+16, ly)
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="12" height="10" class="%s"/><text x="%d" y="%d" class="tick">chunks above host cap (domain_skew &gt; 0)</text>`, padL+130, ly-9, Warn, padL+146, ly)
	b.WriteString("</svg>")
	return b.String()
}
