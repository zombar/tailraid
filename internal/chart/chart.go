// Package chart draws the inline SVG charts of the soak report (docs/09 §6) and the web
// UI's Stats page (docs/13 §Design, docs/17 §4): lines over a shared time axis with phase
// bands and event markers, grouped bars, bars on a log axis, liveness lanes, a strip, and
// the disk-spread lanes.
// Everything is inline, no scripts, no external assets, and no colour: the package emits
// classes only (series s0…s7, status, the spread bins, bands, text and grid), so the
// report's stylesheet and the web UI's app.css each theme the same bytes (I-197). Every
// title, name and tooltip is escaped.
package chart

import (
	"fmt"
	"html"
	"math"
	"sort"
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

// Marker is a vertical event line; its label sits in the lane under the time axis.
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
	Off  Status = "chart-off" // grey: a cell that is neither good nor bad (a relayed path)
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
	Span  float64 // when positive, the x axis ends here (seconds) rather than at the data's last point
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
	"pending", "frame", "note",
	"mesh", "mesh-edge", "lan", "wan", "relay", "none", "mesh-node", "self", "off", "mesh-link", "mesh-name",
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

// markerLabelMax bounds an event label; the tooltip carries it whole.
const markerLabelMax = 40

// The event labels' lane under a line chart's time axis (docs/13 Charts, I-297): three
// rows, always reserved, so a chart is as tall with events as without and the Stats
// page's frame (LineHeight) is the chart's size. A label starts at its marker or slides
// right by at most markShift to clear the label before it in its row; one that fits in no
// row joins the label before it as "+N", every event named in that label's tooltip.
const (
	markRows  = 3
	markRow   = 13
	markLane  = markRows*markRow + 8
	markChar  = 5.6  // a 10 px sans character's mean advance
	markGap   = 8.0  // between two labels in a row
	markShift = 64.0 // the furthest a label leaves its marker
	markPlus  = 20.0 // room kept for a " +N"
)

// LineHeight is the height of a line chart of series with these names: the plot, the
// event lane and the legend.
func LineHeight(names []string) int { return Height + markLane + LegendHeight(names) }

func markBaseline(row int) int { return Height + 4 + (row+1)*markRow }

// markLabel is one label in the lane: the marker's x, where the label starts, its row and
// the events it names (the first is its text).
type markLabel struct {
	x, lx float64
	row   int
	all   []string
}

// placeMarks lays the labels out in marker order (by x, then input order): each takes the
// row that needs the least slide, ties to the upper row; xs are the markers' chart x.
func placeMarks(xs []float64, marks []Marker) []markLabel {
	idx := make([]int, len(marks))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
	var ends [markRows]float64
	for r := range ends {
		ends[r] = math.Inf(-1)
	}
	var out []markLabel
	for _, i := range idx {
		x, label := xs[i], marks[i].Label
		w := float64(len([]rune(truncate(label, markerLabelMax))))*markChar + markPlus
		best, bestLx := -1, 0.0
		for r := range ends {
			lx := math.Max(x, ends[r]+markGap)
			lx = math.Min(lx, float64(Width-padR)-w) // the right edge pulls a label back over its marker
			if lx < ends[r]+markGap || lx-x > markShift {
				continue
			}
			if best < 0 || lx-x < bestLx-x {
				best, bestLx = r, lx
			}
		}
		if best < 0 && len(out) > 0 {
			out[len(out)-1].all = append(out[len(out)-1].all, label)
			continue
		}
		if best < 0 { // the first label of a chart narrower than its text: row 0, at the left
			best, bestLx = 0, math.Max(float64(padL), math.Min(x, float64(Width-padR)-w))
		}
		ends[best] = bestLx + w //nolint:gosec // best is a row the loop above chose, or 0
		out = append(out, markLabel{x: x, lx: bestLx, row: best, all: []string{label}})
	}
	return out
}

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
	if ax.Span > 0 {
		maxX = ax.Span
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
	open(&b, title, Height+markLane+legendHeight(ss))
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
	// markers: the dashed line over the plot, the label in the lane under the time axis
	for _, m := range marks {
		x := sx(m.X)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%d" class="marker"><title>%s</title></line>`, x, padT, x, Height-padB, html.EscapeString(m.Label))
	}
	xs := make([]float64, len(marks))
	for i, m := range marks {
		xs[i] = sx(m.X)
	}
	for _, l := range placeMarks(xs, marks) {
		y := markBaseline(l.row)
		text := truncate(l.all[0], markerLabelMax)
		if len(l.all) > 1 {
			text += fmt.Sprintf(" +%d", len(l.all)-1)
		}
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%d" class="marker"/><text x="%.1f" y="%d" class="marklabel"><title>%s</title>%s</text>`,
			l.x, Height-padB+20, l.lx, y-10, l.lx, y, html.EscapeString(strings.Join(l.all, "\n")), html.EscapeString(text))
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
	legend(&b, ss, Height+markLane)
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

// LegendHeight is the height a line chart's legend adds below the plot for series of
// these names, so a chart's frame can have the drawn chart's size (docs/17 §3).
func LegendHeight(names []string) int {
	ss := make([]Series, len(names))
	for i, n := range names {
		ss[i].Name = n
	}
	return legendHeight(ss)
}

// laneRow is the height of one lane of Lanes.
const laneRow = 16

// LanesHeight is the height of a Lanes chart of n names.
func LanesHeight(n int) int { return padT + laneRow*n + 24 }

// Frame is a chart not drawn yet (docs/17 §3, I-258): the chart's size, its title, a
// dashed frame over the plot area and a "loading" note, classes only. The page draws it
// at once and replaces it with the chart when that arrives, so nothing moves.
func Frame(title string, h int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart pending" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s: loading">`, Width, h, html.EscapeString(title))
	fmt.Fprintf(&b, `<text x="%d" y="18" class="title">%s</text>`, padL, html.EscapeString(title))
	ph := max(h-56, 1)
	fmt.Fprintf(&b, `<rect class="frame" x="%d" y="%d" width="%d" height="%d" rx="2"/>`, padL, padT, Width-padL-padR, ph)
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" class="note">loading</text></svg>`, padL+(Width-padL-padR)/2, padT+ph/2+4)
	return b.String()
}

func legendHeight(ss []Series) int {
	_, _, n := legendRows(ss)
	if n == 0 {
		return 0
	}
	return 6 + n*legendRow
}

// legend draws the series' names in rows from top, under the plot (and a line chart's event lane).
func legend(b *strings.Builder, ss []Series, top int) {
	rows, xs, _ := legendRows(ss)
	for i, s := range ss {
		y := top + 6 + rows[i]*legendRow
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
	legend(&b, ss, Height)
	b.WriteString("</svg>")
	return b.String()
}

// Bar is one bar of LogBars: its label under the axis, its value, its class (s0 when
// empty) and its tooltip.
type Bar struct {
	Label string
	Value float64
	Class string
	Tip   string
}

// LogBars draws one bar per entry on a log₂ axis whose gridlines are 2⁵ apart (1 Mi,
// 32 Mi, 1 Gi for bytes), each bar with its value above it; tick formats an axis or a
// bar value. A value below 1 draws no bar, only its label and value.
func LogBars(title string, bars []Bar, tick func(float64) string) string {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, b := range bars {
		if b.Value >= 1 {
			e := math.Log2(b.Value)
			lo, hi = math.Min(lo, e), math.Max(hi, e)
		}
	}
	if math.IsInf(lo, 1) {
		lo, hi = 0, 5
	}
	lo = math.Floor(lo/5) * 5
	hi = math.Max(math.Ceil(hi/5)*5, lo+5)
	iw, ih := float64(Width-padL-padR), float64(Height-padT-padB)
	sy := func(e float64) float64 { return float64(padT) + ih - (e-lo)/(hi-lo)*ih }
	var b strings.Builder
	open(&b, title, Height)
	for e := lo; e <= hi; e += 5 {
		y := sy(e)
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="grid"/><text x="%d" y="%.1f" class="tick" text-anchor="end">%s</text>`,
			padL, y, Width-padR, y, padL-6, y+4, html.EscapeString(tick(math.Exp2(e))))
	}
	if len(bars) == 0 {
		b.WriteString("</svg>")
		return b.String()
	}
	gw := iw / float64(len(bars))
	for i, bar := range bars {
		x := float64(padL) + gw*float64(i) + gw*0.15
		w := gw * 0.7
		top := sy(lo)
		if bar.Value >= 1 {
			top = sy(math.Max(math.Log2(bar.Value), lo))
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="%s"><title>%s</title></rect>`,
				x, top, w, sy(lo)-top, seriesClass(Series{Class: bar.Class}, 0), html.EscapeString(bar.Tip))
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="tick" text-anchor="middle">%s</text>`, x+w/2, top-6, html.EscapeString(tick(bar.Value)))
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%s</text>`, x+w/2, Height-padB+16, html.EscapeString(truncate(bar.Label, int(gw/6.5)+1)))
	}
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
	const rowH = laneRow
	h := LanesHeight(len(names))
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
