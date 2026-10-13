package chart

import (
	"strconv"
	"strings"
	"testing"
)

// A chart's frame is drawn before its rows are read (docs/17 §3): the chart's own size
// (the viewBox the drawn chart will have), its title, a dashed frame and a "loading"
// note, classes only, the title escaped.
// Invariant: I-258
func TestChart_Frame(t *testing.T) {
	svg := Frame(`a <b> & "c"`, 242)
	for _, want := range []string{
		`<svg class="chart pending" viewBox="0 0 760 242"`,
		`class="title">a &lt;b&gt; &amp; &#34;c&#34;</text>`,
		`<rect class="frame" x="56" y="28" width="688" height="186" rx="2"/>`,
		`class="note">loading</text>`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("the frame lacks %q:\n%s", want, svg)
		}
	}
	known := map[string]bool{}
	for _, c := range Classes {
		known[c] = true
	}
	for _, m := range classAttr.FindAllStringSubmatch(svg, -1) {
		for c := range strings.FieldsSeq(m[1]) {
			if !known[c] {
				t.Errorf("the frame emits class %q, not in Classes", c)
			}
		}
	}
	if strings.Contains(svg, "style=") || colourRe.MatchString(svg) {
		t.Error("the frame carries a style or a colour")
	}
	// the drawn chart of the same title and height has the same viewBox
	line := Line("Chunks", "chunks", []Series{{Name: "live chunks", Points: [][2]float64{{0, 1}, {1, 2}}}}, nil, nil, Axis{})
	h := Height + LegendHeight([]string{"live chunks"})
	if !strings.Contains(line, `viewBox="0 0 760 `+strconv.Itoa(h)+`"`) || !strings.Contains(Frame("Chunks", h), `viewBox="0 0 760 `+strconv.Itoa(h)+`"`) {
		t.Errorf("a line chart and its frame differ in height (want %d)", h)
	}
}
