package soak

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// Small SVG chart helpers: everything is inline, no scripts, no external assets.

type series struct {
	name   string
	color  string
	points [][2]float64 // x (seconds), y
}

type marker struct {
	x     float64
	label string
}

// band is a labelled span of the time axis (a phase).
type band struct {
	x0, x1 float64
	label  string
}

const (
	chartW, chartH = 760, 220
	padL, padR     = 56, 16
	padT, padB     = 28, 36
)

var palette = []string{"#2563eb", "#dc2626", "#16a34a", "#d97706", "#7c3aed", "#0891b2", "#be185d", "#4d7c0f"}

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

// lineChart draws series over a shared time axis with phase bands behind them and
// vertical event markers.
func lineChart(title, yLabel string, ss []series, marks []marker, bands []band) string {
	var maxX, maxY float64
	for _, s := range ss {
		for _, p := range s.points {
			maxX = math.Max(maxX, p[0])
			maxY = math.Max(maxY, p[1])
		}
	}
	for _, m := range marks {
		maxX = math.Max(maxX, m.x)
	}
	for _, b := range bands {
		maxX = math.Max(maxX, b.x1)
	}
	if maxX == 0 {
		maxX = 1
	}
	if maxY == 0 {
		maxY = 1
	}
	maxY *= 1.1
	iw, ih := float64(chartW-padL-padR), float64(chartH-padT-padB)
	sx := func(x float64) float64 { return float64(padL) + x/maxX*iw }
	sy := func(y float64) float64 { return float64(padT) + ih - y/maxY*ih }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`, chartW, chartH+legendHeight(ss), html.EscapeString(title))
	fmt.Fprintf(&b, `<text x="%d" y="18" class="title">%s</text>`, padL, html.EscapeString(title))
	// phase bands, alternating shades, labelled at the top
	for i, bd := range bands {
		x0, x1 := sx(bd.x0), sx(math.Max(bd.x1, bd.x0))
		fill := "#f8fafc"
		if i%2 == 1 {
			fill = "#eef2f7"
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.1f" height="%.1f" fill="%s"><title>%s</title></rect>`, x0, padT, math.Max(x1-x0, 0.5), ih, fill, html.EscapeString(bd.label))
		if x1-x0 > 14 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="marklabel">%s</text>`, x0+2, padT+9, html.EscapeString(bandLabel(bd.label)))
		}
	}
	// grid and axes
	for i := range 5 {
		y := float64(padT) + ih*float64(i)/4
		v := maxY * float64(4-i) / 4
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="grid"/><text x="%d" y="%.1f" class="tick" text-anchor="end">%s</text>`,
			padL, y, chartW-padR, y, padL-6, y+4, fmtNum(v))
	}
	for i := range 7 {
		x := float64(padL) + iw*float64(i)/6
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%ss</text>`, x, chartH-padB+16, fmtNum(maxX*float64(i)/6))
	}
	fmt.Fprintf(&b, `<text x="12" y="%.1f" class="tick" transform="rotate(-90 12 %.1f)" text-anchor="middle">%s</text>`, float64(padT)+ih/2, float64(padT)+ih/2, html.EscapeString(yLabel))
	// markers: labels alternate between three heights so neighbours stay readable
	for i, m := range marks {
		x := sx(m.x)
		y := chartH - padB - 4 - (i%3)*40
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%d" class="marker"><title>%s</title></line><text x="%.1f" y="%d" class="marklabel" transform="rotate(-90 %.1f %d)">%s</text>`,
			x, padT, x, chartH-padB, html.EscapeString(m.label), x+3, y, x+3, y, html.EscapeString(m.label))
	}
	// series
	for i, s := range ss {
		color := s.color
		if color == "" {
			color = palette[i%len(palette)]
		}
		var d strings.Builder
		for j, p := range s.points {
			cmd := "L"
			if j == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, sx(p[0]), sy(p[1]))
		}
		if len(s.points) == 1 {
			p := s.points[0]
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="%s"/>`, sx(p[0]), sy(p[1]), color)
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="1.8"/>`, strings.TrimSpace(d.String()), color)
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
func legendRows(ss []series) (rows []int, xs []float64, nrows int) {
	rows, xs = make([]int, len(ss)), make([]float64, len(ss))
	x, row := float64(padL), 0
	for i, s := range ss {
		w := legendWidth(s.name)
		if x+w > float64(chartW-padR) && x > float64(padL) {
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

func legendHeight(ss []series) int {
	_, _, n := legendRows(ss)
	if n == 0 {
		return 0
	}
	return 6 + n*legendRow
}

func legend(b *strings.Builder, ss []series) {
	rows, xs, _ := legendRows(ss)
	for i, s := range ss {
		color := s.color
		if color == "" {
			color = palette[i%len(palette)]
		}
		y := chartH + 6 + rows[i]*legendRow
		fmt.Fprintf(b, `<rect x="%.1f" y="%d" width="%d" height="%d" fill="%s"/><text x="%.1f" y="%d" class="tick">%s</text>`,
			xs[i], y, legendGlyph, legendGlyph, color, xs[i]+legendGlyph+6, y+legendGlyph-1, html.EscapeString(s.name))
	}
}

// barChart draws grouped bars: one group per label, one bar per series.
func barChart(title string, labels []string, ss []series) string {
	var maxY float64
	for _, s := range ss {
		for _, p := range s.points {
			maxY = math.Max(maxY, p[1])
		}
	}
	if maxY == 0 {
		maxY = 1
	}
	maxY *= 1.1
	iw, ih := float64(chartW-padL-padR), float64(chartH-padT-padB)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`, chartW, chartH+legendHeight(ss), html.EscapeString(title))
	fmt.Fprintf(&b, `<text x="%d" y="18" class="title">%s</text>`, padL, html.EscapeString(title))
	for i := range 5 {
		y := float64(padT) + ih*float64(i)/4
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="grid"/><text x="%d" y="%.1f" class="tick" text-anchor="end">%s</text>`,
			padL, y, chartW-padR, y, padL-6, y+4, fmtNum(maxY*float64(4-i)/4))
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
			if li >= len(s.points) {
				continue
			}
			color := s.color
			if color == "" {
				color = palette[si%len(palette)]
			}
			h := s.points[li][1] / maxY * ih
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"><title>%s %s: %s</title></rect>`,
				gx+bw*float64(si), float64(padT)+ih-h, bw-1, h, color, html.EscapeString(s.name), html.EscapeString(label), fmtNum(s.points[li][1]))
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%s</text>`, gx+gw*0.4, chartH-padB+16, html.EscapeString(label))
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

// lanes draws one row of coloured cells per name along the time axis (node liveness).
func lanes(title string, maxX float64, names []string, cells map[string][][2]float64, colors map[string][]string) string {
	const rowH = 16
	h := padT + rowH*len(names) + 24
	iw := float64(chartW - padL - padR)
	if maxX == 0 {
		maxX = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`, chartW, h, html.EscapeString(title))
	fmt.Fprintf(&b, `<text x="%d" y="18" class="title">%s</text>`, padL, html.EscapeString(title))
	for row, name := range names {
		y := padT + row*rowH
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="tick" text-anchor="end">%s</text>`, padL-6, y+12, html.EscapeString(name))
		for i, c := range cells[name] {
			x := float64(padL) + c[0]/maxX*iw
			fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="4" height="%d" fill="%s"/>`, x-2, y+2, rowH-4, colors[name][i])
		}
	}
	for i := range 7 {
		x := float64(padL) + iw*float64(i)/6
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%ss</text>`, x, h-6, fmtNum(maxX*float64(i)/6))
	}
	b.WriteString("</svg>")
	return b.String()
}

// strip draws a row of coloured cells along the time axis (FSM-hash agreement).
func strip(title string, maxX float64, cells [][2]float64, colors []string) string {
	const h = 70
	iw := float64(chartW - padL - padR)
	if maxX == 0 {
		maxX = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`, chartW, h, html.EscapeString(title))
	fmt.Fprintf(&b, `<text x="%d" y="18" class="title">%s</text>`, padL, html.EscapeString(title))
	for i, c := range cells {
		x := float64(padL) + c[0]/maxX*iw
		fmt.Fprintf(&b, `<rect x="%.1f" y="28" width="6" height="20" fill="%s"/>`, x-3, colors[i])
	}
	for i := range 7 {
		x := float64(padL) + iw*float64(i)/6
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%ss</text>`, x, h-6, fmtNum(maxX*float64(i)/6))
	}
	b.WriteString("</svg>")
	return b.String()
}
