// Package chart draws the inline SVG charts of the soak report (docs/09 §6) and the web
// UI's Stats page (docs/13 §Design, docs/17 §4): lines over a shared time axis with phase
// bands and event markers, grouped bars, liveness lanes, a strip, and the disk-spread lanes.
// Everything is inline, no scripts, no external assets, and no colour: the package emits
// classes only (series s0…s7, status, the spread bins, bands, text and grid), so the
// report's stylesheet and the web UI's app.css each theme the same bytes (I-197). Every
// title, name and tooltip is escaped.
package chart

import (
	"fmt"
	"html"
	"math"
	"strings"
	"time"
)

// Series is one line or one bar group: Name is the legend entry, Class the series class
// (s<i> by its position when empty), Points are (x seconds, y).
type Series struct {
	Name   string
	Class  string
	Points [][2]float64
}

// Marker is a vertical event line with a rotated label.
type Marker struct {
	X     float64
	Label string
}

// Band is a labelled span of the time axis (a phase).
type Band struct {
	X0, X1 float64
	Label  string
}

// Status is the class of a lane or strip cell.
type Status string

// The status classes: the web UI maps them to its status tokens, the report to its greens,
// ambers and reds.
const (
	OK   Status = "chart-ok"
	Warn Status = "chart-warn"
	Bad  Status = "chart-bad"
)

// Cell is one coloured cell of a lane or a strip at x.
type Cell struct {
	X      float64
	Status Status
}

// Axis is how the time axis prints: seconds since the start (the report) or clock times
// from Start (the Stats page): HH:MM within a day, MM-DD HH:MM beyond.
type Axis struct {
	Clock bool
	Start time.Time
}

// tick is an axis tick label for x over a span of maxX seconds.
func (a Axis) tick(x, maxX float64) string {
	if !a.Clock {
		return fmtNum(x) + "s"
	}
	return a.clock(x, maxX)
}

// at is a tooltip's instant for x.
func (a Axis) at(x, maxX float64) string {
	if !a.Clock {
		return fmt.Sprintf("%.0fs", x)
	}
	return a.clock(x, maxX)
}

func (a Axis) clock(x, maxX float64) string {
	t := a.Start.Add(time.Duration(math.Round(x)) * time.Second).UTC()
	if maxX > 86400 {
		return t.Format("01-02 15:04")
	}
	return t.Format("15:04")
}

// The geometry, in the SVG's own units; a chart scales to its container's width.
const (
	Width  = 760
	Height = 220
	padL   = 56
	padR   = 16
	padT   = 28
	padB   = 36
)

// Points is the most points a series needs: one per unit of the chart's width.
const Points = Width

// Classes is every class the package can emit; app.css and the report's stylesheet define
// each (I-197).
var Classes = []string{
	"chart", "title", "tick", "grid", "marker", "marklabel", "band0", "band1",
	"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7",
	string(OK), string(Warn), string(Bad), "chart-bg", "chart-off",
	"dev-3", "dev-2", "dev-1", "dev0", "dev1", "dev2", "dev3",
}

func seriesClass(s Series, i int) string {
	if s.Class != "" {
		return s.Class
	}
	return fmt.Sprintf("s%d", i%8)
}

func fmtNum(v float64) string {
	switch {
	case v == 0:
		return "0"
	case math.Abs(v) >= 1e9:
		return fmt.Sprintf("%.1fG", v/1e9)
	case math.Abs(v) >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case math.Abs(v) >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case math.Abs(v) < 10:
		return fmt.Sprintf("%.2g", v)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// markerLabelMax bounds a rotated event label so it stays inside the chart.
const markerLabelMax = 40

// truncate cuts s to n runes with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// finitePoints drops NaN and Inf points: one bad vital from a node would otherwise make
// every coordinate of the chart NaN.
func finitePoints(ss []Series) []Series {
	out := make([]Series, len(ss))
	for i, s := range ss {
		out[i] = s
		out[i].Points = nil
		for _, p := range s.Points {
			if math.IsNaN(p[0]) || math.IsInf(p[0], 0) || math.IsNaN(p[1]) || math.IsInf(p[1], 0) {
				continue
			}
			out[i].Points = append(out[i].Points, p)
		}
	}
	return out
}

func open(b *strings.Builder, title string, h int) {
	fmt.Fprintf(b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`, Width, h, html.EscapeString(title))
	fmt.Fprintf(b, `<text x="%d" y="18" class="title">%s</text>`, padL, html.EscapeString(title))
}

func xTicks(b *strings.Builder, y int, maxX float64, ax Axis) {
	iw := float64(Width - padL - padR)
	for i := range 7 {
		x := float64(padL) + iw*float64(i)/6
		fmt.Fprintf(b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%s</text>`, x, y, ax.tick(maxX*float64(i)/6, maxX))
	}
}

// Line draws series over a shared time axis with phase bands behind them and vertical
// event markers.
func Line(title, yLabel string, ss []Series, marks []Marker, bands []Band, ax Axis) string {
	ss = finitePoints(ss)
	var maxX, maxY float64
	for _, s := range ss {
		for _, p := range s.Points {
			maxX = math.Max(maxX, p[0])
			maxY = math.Max(maxY, p[1])
		}
	}
	for _, m := range marks {
		maxX = math.Max(maxX, m.X)
	}
	for _, bd := range bands {
		maxX = math.Max(maxX, bd.X1)
	}
	if maxX == 0 {
		maxX = 1
	}
	if maxY == 0 {
		maxY = 1
	}
	maxY *= 1.1
	iw, ih := float64(Width-padL-padR), float64(Height-padT-padB)
	sx := func(x float64) float64 { return float64(padL) + x/maxX*iw }
	sy := func(y float64) float64 { return float64(padT) + ih - y/maxY*ih }
	var b strings.Builder
	open(&b, title, Height+legendHeight(ss))
	// phase bands, alternating shades, labelled at the top
	for i, bd := range bands {
		x0, x1 := sx(bd.X0), sx(math.Max(bd.X1, bd.X0))
		fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.1f" height="%.1f" class="band%d"><title>%s</title></rect>`, x0, padT, math.Max(x1-x0, 0.5), ih, i%2, html.EscapeString(bd.Label))
		if x1-x0 > 14 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="marklabel">%s</text>`, x0+2, padT+9, html.EscapeString(bandLabel(bd.Label)))
		}
	}
	// grid and axes
	for i := range 5 {
		y := float64(padT) + ih*float64(i)/4
		v := maxY * float64(4-i) / 4
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="grid"/><text x="%d" y="%.1f" class="tick" text-anchor="end">%s</text>`,
			padL, y, Width-padR, y, padL-6, y+4, fmtNum(v))
	}
	xTicks(&b, Height-padB+16, maxX, ax)
	fmt.Fprintf(&b, `<text x="12" y="%.1f" class="tick" transform="rotate(-90 12 %.1f)" text-anchor="middle">%s</text>`, float64(padT)+ih/2, float64(padT)+ih/2, html.EscapeString(yLabel))
	// markers: labels alternate between three heights so neighbours stay readable
	for i, m := range marks {
		x := sx(m.X)
		y := Height - padB - 4 - (i%3)*40
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%d" class="marker"><title>%s</title></line><text x="%.1f" y="%d" class="marklabel" transform="rotate(-90 %.1f %d)">%s</text>`,
			x, padT, x, Height-padB, html.EscapeString(m.Label), x+3, y, x+3, y, html.EscapeString(truncate(m.Label, markerLabelMax)))
	}
	// series
	for i, s := range ss {
		cls := seriesClass(s, i)
		var d strings.Builder
		for j, p := range s.Points {
			cmd := "L"
			if j == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, sx(p[0]), sy(p[1]))
		}
		if len(s.Points) == 1 {
			p := s.Points[0]
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" class="%s"/>`, sx(p[0]), sy(p[1]), cls)
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" class="%s" stroke-width="1.8"/>`, strings.TrimSpace(d.String()), cls)
	}
	legend(&b, ss)
	b.WriteString("</svg>")
	return b.String()
}

// The legend sits below the plot, outside the drawing, in rows that wrap at the chart's
// width, so no series ever covers the data.
const (
	legendRow   = 16
	legendGap   = 18
	legendGlyph = 10
)

// legendWidth estimates an entry's width in the chart's units (the tick font is ~6.5 px
// per character).
func legendWidth(name string) float64 { return legendGlyph + 6 + float64(len(name))*6.5 + legendGap }

// legendRows lays the entries out: each entry's row and x.
func legendRows(ss []Series) (rows []int, xs []float64, nrows int) {
	rows, xs = make([]int, len(ss)), make([]float64, len(ss))
	x, row := float64(padL), 0
	for i, s := range ss {
		w := legendWidth(s.Name)
		if x+w > float64(Width-padR) && x > float64(padL) {
			x, row = float64(padL), row+1
		}
		rows[i], xs[i] = row, x
		x += w
	}
	if len(ss) > 0 {
		nrows = row + 1
	}
	return rows, xs, nrows
}

func legendHeight(ss []Series) int {
	_, _, n := legendRows(ss)
	if n == 0 {
		return 0
	}
	return 6 + n*legendRow
}

func legend(b *strings.Builder, ss []Series) {
	rows, xs, _ := legendRows(ss)
	for i, s := range ss {
		y := Height + 6 + rows[i]*legendRow
		fmt.Fprintf(b, `<rect x="%.1f" y="%d" width="%d" height="%d" class="%s"/><text x="%.1f" y="%d" class="tick">%s</text>`,
			xs[i], y, legendGlyph, legendGlyph, seriesClass(s, i), xs[i]+legendGlyph+6, y+legendGlyph-1, html.EscapeString(s.Name))
	}
}

// Bars draws grouped bars: one group per label, one bar per series.
func Bars(title string, labels []string, ss []Series) string {
	var maxY float64
	for _, s := range ss {
		for _, p := range s.Points {
			maxY = math.Max(maxY, p[1])
		}
	}
	if maxY == 0 {
		maxY = 1
	}
	maxY *= 1.1
	iw, ih := float64(Width-padL-padR), float64(Height-padT-padB)
	var b strings.Builder
	open(&b, title, Height+legendHeight(ss))
	for i := range 5 {
		y := float64(padT) + ih*float64(i)/4
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="grid"/><text x="%d" y="%.1f" class="tick" text-anchor="end">%s</text>`,
			padL, y, Width-padR, y, padL-6, y+4, fmtNum(maxY*float64(4-i)/4))
	}
	if len(labels) == 0 {
		b.WriteString("</svg>")
		return b.String()
	}
	gw := iw / float64(len(labels))
	bw := gw * 0.8 / float64(max(len(ss), 1))
	for li, label := range labels {
		gx := float64(padL) + gw*float64(li) + gw*0.1
		for si, s := range ss {
			if li >= len(s.Points) {
				continue
			}
			h := s.Points[li][1] / maxY * ih
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="%s"><title>%s %s: %s</title></rect>`,
				gx+bw*float64(si), float64(padT)+ih-h, bw-1, h, seriesClass(s, si), html.EscapeString(s.Name), html.EscapeString(label), fmtNum(s.Points[li][1]))
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%s</text>`, gx+gw*0.4, Height-padB+16, html.EscapeString(label))
	}
	legend(&b, ss)
	b.WriteString("</svg>")
	return b.String()
}

// bandLabel shortens "4 parity" to its number for narrow bands.
func bandLabel(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 && i <= 2 {
		return s[:i]
	}
	return s
}

// Lanes draws one row of status cells per name along the time axis (node liveness).
func Lanes(title string, maxX float64, names []string, cells map[string][]Cell, ax Axis) string {
	const rowH = 16
	h := padT + rowH*len(names) + 24
	iw := float64(Width - padL - padR)
	if maxX == 0 {
		maxX = 1
	}
	var b strings.Builder
	open(&b, title, h)
	for row, name := range names {
		y := padT + row*rowH
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="tick" text-anchor="end">%s</text>`, padL-6, y+12, html.EscapeString(name))
		for _, c := range cells[name] {
			x := float64(padL) + c.X/maxX*iw
			fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="4" height="%d" class="%s"/>`, x-2, y+2, rowH-4, c.Status)
		}
	}
	xTicks(&b, h-6, maxX, ax)
	b.WriteString("</svg>")
	return b.String()
}

// Strip draws a row of status cells along the time axis (FSM-hash agreement).
func Strip(title string, maxX float64, cells []Cell, ax Axis) string {
	const h = 70
	iw := float64(Width - padL - padR)
	if maxX == 0 {
		maxX = 1
	}
	var b strings.Builder
	open(&b, title, h)
	for _, c := range cells {
		x := float64(padL) + c.X/maxX*iw
		fmt.Fprintf(&b, `<rect x="%.1f" y="28" width="6" height="20" class="%s"/>`, x-3, c.Status)
	}
	xTicks(&b, h-6, maxX, ax)
	b.WriteString("</svg>")
	return b.String()
}
