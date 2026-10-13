package webui_test

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The stylesheet's rules (docs/13 §Design, Space and shape, Layout, Icons) checked by
// reading app.css: no browser runs here, so these are the rules a reader of the sheet can
// verify, and a layout that breaks between two widths shows up as a value off the grid, a
// width query that is not a window class, a grid minimum that does not clamp, or a control
// only a mouse can reveal.

// cssRule is one style rule: the at-rule preludes around it, its selector and its
// declarations in order.
type cssRule struct {
	at    []string
	sel   string
	decls [][2]string
}

func (r cssRule) in(sub string) bool {
	for _, a := range r.at {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

func (r cssRule) selectors() []string {
	var out []string
	for s := range strings.SplitSeq(r.sel, ",") {
		out = append(out, strings.Join(strings.Fields(s), " "))
	}
	return out
}

func (r cssRule) get(prop string) (string, bool) {
	for _, d := range r.decls {
		if d[0] == prop {
			return d[1], true
		}
	}
	return "", false
}

// parseCSS splits a stylesheet into its rules. It knows the sheet's own shapes: plain
// rules, @media blocks (nested), @keyframes (whose steps it returns under the keyframes
// prelude) and comments; strings never hold braces or semicolons in app.css.
func parseCSS(css string) []cssRule {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var out []cssRule
	var stack []string
	var buf strings.Builder
	for _, r := range css {
		switch r {
		case '{':
			stack = append(stack, strings.TrimSpace(buf.String()))
			buf.Reset()
		case '}':
			if len(stack) == 0 {
				continue
			}
			sel := stack[len(stack)-1]
			body := buf.String()
			buf.Reset()
			stack = stack[:len(stack)-1]
			if strings.HasPrefix(sel, "@") {
				continue
			}
			rule := cssRule{at: slices.Clone(stack), sel: sel}
			for d := range strings.SplitSeq(body, ";") {
				prop, val, ok := strings.Cut(d, ":")
				if !ok {
					continue
				}
				rule.decls = append(rule.decls, [2]string{strings.TrimSpace(prop), strings.Join(strings.Fields(val), " ")})
			}
			out = append(out, rule)
		default:
			buf.WriteRune(r)
		}
	}
	return out
}

func appCSS(t *testing.T) []cssRule {
	t.Helper()
	return parseCSS(readAll(t, filepath.Join(srcDir, "app.css")))
}

// lookdevStyles returns each lookdev page's <style> block. The soak-* pages draw the soak
// report, whose own sheet has fixed colours (docs/09 §6), so only the width queries and the
// glyphs hold for them.
func lookdevStyles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range glob(t, filepath.Join(lookdevDir, "*.html")) {
		html := readAll(t, p)
		for _, m := range regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(html, -1) {
			out[filepath.Base(p)] += m[1] + "\n"
		}
	}
	return out
}

// The three token blocks: the light block (also the lookdev's .theme-light frame), the dark
// block under prefers-color-scheme and the dark block under data-theme (I-200).
const (
	lightSel     = ":root, .theme-light"
	darkMediaSel = `:root:not([data-theme="light"])`
	darkAttrSel  = `:root[data-theme="dark"], .theme-dark`
)

func isTokenBlock(r cssRule) bool {
	return r.sel == lightSel || r.sel == darkMediaSel || r.sel == darkAttrSel
}

func tokens(t *testing.T, rules []cssRule, sel string) map[string]string {
	t.Helper()
	var found []cssRule
	for _, r := range rules {
		if r.sel == sel {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("token block %q: %d rules, want 1", sel, len(found))
	}
	out := map[string]string{}
	for _, d := range found[0].decls {
		if _, dup := out[d[0]]; dup {
			t.Errorf("token block %q sets %s twice", sel, d[0])
		}
		out[d[0]] = d[1]
	}
	return out
}

var (
	hexColour   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	funcColour  = regexp.MustCompile(`\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch)\(`)
	namedColour = regexp.MustCompile(`(?i)(?:^|[\s,(])(white|black|red|green|blue|gray|grey|silver|orange|yellow|purple|navy|maroon|teal|olive|lime|aqua|fuchsia)(?:$|[\s,)])`)
)

func hasColour(v string) bool {
	return hexColour.MatchString(v) || funcColour.MatchString(v) || namedColour.MatchString(v)
}

// Every colour is a token: a value outside the three token blocks names a colour only
// through var(--…), so both themes reach every rule.
// Invariant: I-283
func TestCSS_ColoursOnlyInTokens(t *testing.T) {
	// The one literal: the hero's fade is an alpha mask, where only the alpha counts.
	allowed := map[string]bool{".hero img.backdrop|mask-image": true}
	check := func(name string, rules []cssRule) {
		for _, r := range rules {
			if isTokenBlock(r) {
				for _, d := range r.decls {
					if !strings.HasPrefix(d[0], "--") && d[0] != "color-scheme" {
						t.Errorf("%s: token block %q sets %s; it holds custom properties only", name, r.sel, d[0])
					}
				}
				continue
			}
			for _, d := range r.decls {
				if hasColour(d[1]) && !allowed[r.sel+"|"+d[0]] {
					t.Errorf("%s: %s { %s: %s } names a colour; use a token", name, r.sel, d[0], d[1])
				}
			}
		}
	}
	check("app.css", appCSS(t))
	for page, css := range lookdevStyles(t) {
		if !strings.HasPrefix(page, "soak-") {
			check(page, parseCSS(css))
		}
	}
}

func TestCSS_HasColour_Table(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"#fff", true},
		{"#0f172a", true},
		{"rgba(0, 0, 0, .25)", true},
		{"0 4px 16px rgb(0 0 0)", true},
		{"white", true},
		{"1px solid black", true},
		{"hsl(0 0% 0%)", true},
		{"var(--bg)", false},
		{"transparent", false},
		{"currentColor", false},
		{"color-mix(in srgb, var(--warn) 12%, var(--bg))", false},
		{"#toasts", false},
		{"1px solid var(--line)", false},
		{"none", false},
		{"whitespace", false},
	} {
		if got := hasColour(c.v); got != c.want {
			t.Errorf("hasColour(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

// Each theme defines every colour token: the dark block under prefers-color-scheme and the
// one under data-theme="dark" are the same map (CSS cannot share one block between a media
// query and an attribute selector, so the sheet writes it twice and this test keeps the two
// copies equal), and every colour the light block sets is either redefined for dark or
// listed here as the same in both.
// Invariant: I-284
func TestCSS_ThemesDefineTheSameTokens(t *testing.T) {
	rules := appCSS(t)
	light := tokens(t, rules, lightSel)
	darkMedia := tokens(t, rules, darkMediaSel)
	darkAttr := tokens(t, rules, darkAttrSel)

	for _, r := range rules {
		if r.sel == darkMediaSel && (len(r.at) != 1 || r.at[0] != "@media (prefers-color-scheme: dark)") {
			t.Errorf("%s sits under %v, want only @media (prefers-color-scheme: dark)", darkMediaSel, r.at)
		}
	}
	for k, v := range darkMedia {
		if darkAttr[k] != v {
			t.Errorf("dark token %s: %q under prefers-color-scheme, %q under data-theme", k, v, darkAttr[k])
		}
	}
	for k, v := range darkAttr {
		if _, ok := darkMedia[k]; !ok {
			t.Errorf("dark token %s = %q is set under data-theme only", k, v)
		}
	}
	// The status colours pass the validator on both grounds (docs/13 Tokens), and the ink on
	// a status cell and the ground behind a video are the same in both themes.
	same := map[string]bool{"--ok": true, "--warn": true, "--bad": true, "--on-status": true, "--media-bg": true, "--scrim": true, "--overlay": true}
	for k, v := range light {
		if !hasColour(v) {
			continue
		}
		_, dark := darkMedia[k]
		switch {
		case !dark && !same[k]:
			t.Errorf("light colour token %s = %q has no dark value (or add it to the same-in-both list with docs/13's reason)", k, v)
		case dark && same[k]:
			t.Errorf("token %s is listed as the same in both themes but the dark block redefines it", k)
		}
	}
	for k := range darkMedia {
		if _, ok := light[k]; !ok && k != "color-scheme" {
			t.Errorf("dark token %s has no light value", k)
		}
	}
}

var varUse = regexp.MustCompile(`var\(\s*(--[\w-]+)`)

// Every var(--x) the sheet, the lookdev and the TypeScript use is defined, and every custom
// property the sheet defines is used.
// Invariant: I-285
func TestCSS_VarsDefinedAndUsed(t *testing.T) {
	rules := appCSS(t)
	defined := map[string]bool{}
	used := map[string]bool{}
	for _, r := range rules {
		for _, d := range r.decls {
			if strings.HasPrefix(d[0], "--") {
				defined[d[0]] = true
			}
			for _, m := range varUse.FindAllStringSubmatch(d[1], -1) {
				used[m[1]] = true
			}
		}
	}
	elsewhere := map[string]bool{}
	for page, css := range lookdevStyles(t) {
		local := map[string]bool{}
		for _, r := range parseCSS(css) {
			for _, d := range r.decls {
				if strings.HasPrefix(d[0], "--") {
					local[d[0]] = true
				}
			}
		}
		for _, m := range varUse.FindAllStringSubmatch(css, -1) {
			elsewhere[m[1]] = true
			if !defined[m[1]] && !local[m[1]] {
				t.Errorf("%s uses %s, which app.css does not define", page, m[1])
			}
		}
	}
	markup := readAll(t, append(glob(t, filepath.Join(lookdevDir, "*.html")), staticHTML)...)
	ts := readAll(t, glob(t, filepath.Join(srcDir, "*.ts"))...)
	for _, m := range varUse.FindAllStringSubmatch(markup+ts, -1) {
		elsewhere[m[1]] = true
		if !defined[m[1]] {
			t.Errorf("markup or TypeScript uses %s, which app.css does not define", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`setProperty\("(--[\w-]+)"`).FindAllStringSubmatch(ts, -1) {
		elsewhere[m[1]] = true
	}
	for k := range used {
		if !defined[k] {
			t.Errorf("app.css uses %s, which it does not define", k)
		}
	}
	var dead []string
	for k := range defined {
		if !used[k] && !elsewhere[k] {
			dead = append(dead, k)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("app.css defines custom properties nothing uses: %v", dead)
	}
}

var pxNum = regexp.MustCompile(`(-?\d*\.?\d+)px`)

// spacingProps are the properties that place things apart: Material's 4 px baseline holds
// for each.
func spacingProp(p string) bool {
	for _, pre := range []string{"padding", "margin", "inset"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	switch p {
	case "gap", "row-gap", "column-gap", "top", "right", "bottom", "left":
		return true
	}
	return false
}

// onGrid reports whether a px length is on the 4 px grid or a hairline (1 or 2 px: a
// border's width, a focus offset, the inner step of a dense row).
func onGrid(px float64) bool {
	if px < 0 {
		px = -px
	}
	return px == 0 || px == 1 || px == 2 || (px == float64(int(px)) && int(px)%4 == 0)
}

func TestCSS_OnGrid_Table(t *testing.T) {
	for _, c := range []struct {
		px   float64
		want bool
	}{
		{0, true},
		{1, true},
		{1.5, false},
		{2, true},
		{3, false},
		{4, true},
		{5, false},
		{6, false},
		{8, true},
		{10, false},
		{12, true},
		{14, false},
		{16, true},
		{18, false},
		{24, true},
		{-4, true},
		{-6, false},
	} {
		if got := onGrid(c.px); got != c.want {
			t.Errorf("onGrid(%v) = %v, want %v", c.px, got, c.want)
		}
	}
}

// Spacing sits on the 4 px grid: every padding, margin, gap and offset in px is a multiple
// of 4 or a 1–2 px hairline. A value that centres a shape on a line (half its own size) is
// geometry, not spacing, and is listed here with the shape it centres.
// Invariant: I-286
func TestCSS_SpacingOnGrid(t *testing.T) {
	geometry := map[string]string{
		".spark-dot|right":         "half the 10 px end dot, centring it on the line's last point",
		".spark-dot|margin-top":    "half the 10 px end dot",
		".spark-wrap|margin-right": "half the end dot, so the dot that overhangs the line's end stays inside the card",
	}
	check := func(name string, rules []cssRule) {
		for _, r := range rules {
			if isTokenBlock(r) {
				continue
			}
			for _, d := range r.decls {
				if !spacingProp(d[0]) || geometry[r.sel+"|"+d[0]] != "" {
					continue
				}
				for _, m := range pxNum.FindAllStringSubmatch(d[1], -1) {
					v, err := strconv.ParseFloat(m[1], 64)
					if err != nil {
						t.Fatal(err)
					}
					if !onGrid(v) {
						t.Errorf("%s: %s { %s: %s }: %spx is off the 4 px grid (docs/13 Space and shape)", name, r.sel, d[0], d[1], m[1])
					}
				}
			}
		}
	}
	check("app.css", appCSS(t))
	for page, css := range lookdevStyles(t) {
		if !strings.HasPrefix(page, "soak-") {
			check(page, parseCSS(css))
		}
	}
	// the spacing tokens are the grid's steps
	for k, v := range tokens(t, appCSS(t), lightSel) {
		if strings.HasPrefix(k, "--sp-") || k == "--gutter" || k == "--pad" {
			for _, m := range pxNum.FindAllStringSubmatch(v, -1) {
				if f, _ := strconv.ParseFloat(m[1], 64); !onGrid(f) || f < 4 {
					t.Errorf("spacing token %s = %s is not a 4 px step", k, v)
				}
			}
		}
	}
}

// windowClasses are Material's window size classes (compact < 600, medium 600–839,
// expanded 840–1199, large ≥ 1200) as the only width queries the UI asks.
var windowClasses = map[string]bool{
	"(max-width: 599px)":                        true,
	"(min-width: 600px)":                        true,
	"(min-width: 600px) and (max-width: 839px)": true,
	"(max-width: 839px)":                        true,
	"(min-width: 840px)":                        true,
	"(max-width: 1199px)":                       true,
	"(min-width: 1200px)":                       true,
}

// Every width query is a window class, in the sheet and on every lookdev page.
// Invariant: I-287
func TestCSS_BreakpointsAreWindowClasses(t *testing.T) {
	media := regexp.MustCompile(`@media\s*([^{]*)\{`)
	check := func(name, css string) {
		css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
		for _, m := range media.FindAllStringSubmatch(css, -1) {
			q := strings.Join(strings.Fields(m[1]), " ")
			if !strings.Contains(q, "width") {
				continue
			}
			if !windowClasses[q] {
				t.Errorf("%s: @media %s is not a window class (docs/13 Layout: 600, 840, 1200)", name, q)
			}
		}
	}
	check("app.css", readAll(t, filepath.Join(srcDir, "app.css")))
	for page, css := range lookdevStyles(t) {
		check(page, css)
	}
}

// interactive matches a selector whose subject is something a finger presses.
var interactive = regexp.MustCompile(`(?:^|[\s>+~])(?:button|summary|select|\.btn|\.menu-row|\.theme|\.play|\.queue-btn|\.icon-btn)(?:[.:\[][^\s]*)?$|input\[type=(?:text|search|datetime-local)\][^\s]*$|nav\.tabs a$|\.picker-list li$`)

// Touch-sized by default: a control's min-height under 48 px (Material's touch target)
// sits only inside @media (pointer: fine), where a mouse can hit the denser size.
// Invariant: I-288
func TestCSS_DenseTargetsOnlyForFinePointers(t *testing.T) {
	var base []string
	for _, r := range appCSS(t) {
		v, ok := r.get("min-height")
		if !ok {
			continue
		}
		m := pxNum.FindStringSubmatch(v)
		if m == nil {
			continue
		}
		px, _ := strconv.ParseFloat(m[1], 64)
		for _, s := range r.selectors() {
			if !interactive.MatchString(s) {
				continue
			}
			if px < 48 && !r.in("pointer: fine") {
				t.Errorf("%s { min-height: %s } is below the 48 px touch target outside @media (pointer: fine)", s, v)
			}
			if s == "button" && len(r.at) == 0 {
				base = append(base, v)
			}
		}
	}
	if len(base) == 0 {
		t.Error("no base button rule sets a min-height")
	}
}

func TestCSS_Interactive_Table(t *testing.T) {
	for _, c := range []struct {
		sel  string
		want bool
	}{
		{"button", true},
		{".btn", true},
		{".seg button", true},
		{"nav.tabs a", true},
		{"details.menu > summary", true},
		{"form.search input[type=search]", true},
		{".nowbar .transport button", true},
		{"table.files td.name .play", true},
		{".menu-row input", false},
		{".cell", false},
		{".pill", false},
		{".picker-list li", true},
		{"button:hover", true},
		{".theme svg.icon", false},
	} {
		if got := interactive.MatchString(c.sel); got != c.want {
			t.Errorf("interactive(%q) = %v, want %v", c.sel, got, c.want)
		}
	}
}

// A control revealed on hover is shown without it on a device that cannot hover: every
// selector hidden with visibility: hidden is made visible under @media (hover: none).
// Invariant: I-289
func TestCSS_HoverRevealHasTouchFallback(t *testing.T) {
	rules := appCSS(t)
	shown := map[string]bool{}
	for _, r := range rules {
		if v, _ := r.get("visibility"); v == "visible" && r.in("hover: none") {
			for _, s := range r.selectors() {
				shown[s] = true
			}
		}
	}
	for _, r := range rules {
		if v, _ := r.get("visibility"); v != "hidden" || len(r.at) > 0 {
			continue
		}
		for _, s := range r.selectors() {
			if !shown[s] {
				t.Errorf("%s is hidden until hover; show it under @media (hover: none)", s)
			}
		}
	}
}

// An auto-fill or auto-fit grid clamps its minimum to the container, so one column fits a
// 320 px screen without a media query: minmax(min(Npx, 100%), 1fr).
// Invariant: I-290
func TestCSS_AutoFillMinimumsClamp(t *testing.T) {
	re := regexp.MustCompile(`repeat\(\s*auto-fi(?:ll|t)\s*,\s*minmax\(\s*([^,]+?)\s*,`)
	check := func(name string, rules []cssRule) {
		for _, r := range rules {
			for _, d := range r.decls {
				for _, m := range re.FindAllStringSubmatch(d[1], -1) {
					if !strings.HasPrefix(m[1], "min(") || !strings.Contains(d[1], "100%)") {
						t.Errorf("%s: %s { %s: %s }: clamp the minimum, minmax(min(%s, 100%%), …)", name, r.sel, d[0], d[1], m[1])
					}
				}
			}
		}
	}
	check("app.css", appCSS(t))
	for page, css := range lookdevStyles(t) {
		if !strings.HasPrefix(page, "soak-") {
			check(page, parseCSS(css))
		}
	}
}

// z-index takes one of the named layers (docs/13 Layout): 1 a control over its tile, 3 the
// bottom bars and toasts, 4 the menus, 5 the sticky header.
// Invariant: I-291
func TestCSS_ZLayers(t *testing.T) {
	layers := map[string]bool{"1": true, "3": true, "4": true, "5": true}
	for _, r := range appCSS(t) {
		if v, ok := r.get("z-index"); ok && !layers[v] {
			t.Errorf("%s { z-index: %s } is not a layer of docs/13 Layout (1, 3, 4, 5)", r.sel, v)
		}
	}
}

// Font sizes come from the type scale of docs/13 Type (chart labels 9 and 10 px, the
// warning glyph 10 px).
// Invariant: I-292
func TestCSS_FontScale(t *testing.T) {
	scale := map[string]bool{"9": true, "10": true, "11": true, "12": true, "13": true, "14": true, "15": true, "17": true, "18": true, "20": true, "22": true}
	size := regexp.MustCompile(`(?:^|\s)(\d+(?:\.\d+)?)px(?:/|\s|$)`)
	for _, r := range appCSS(t) {
		for _, d := range r.decls {
			if d[0] != "font-size" && d[0] != "font" {
				continue
			}
			m := size.FindStringSubmatch(d[1])
			if m == nil {
				continue
			}
			if !scale[m[1]] {
				t.Errorf("%s { %s: %s }: %spx is not on the type scale", r.sel, d[0], d[1], m[1])
			}
		}
	}
}

// A rule sets each property once: a second copy is paste drift, and the later one silently
// wins.
func TestCSS_NoPropertyTwiceInARule(t *testing.T) {
	for _, r := range appCSS(t) {
		seen := map[string]bool{}
		for _, d := range r.decls {
			if seen[d[0]] {
				t.Errorf("%s sets %s twice", r.sel, d[0])
			}
			seen[d[0]] = true
		}
	}
}

func TestCSS_Parse(t *testing.T) {
	rules := parseCSS(`/* c { } */ a, b { x: 1; y: calc(2px + 4px) }
@media (max-width: 599px) { .c { z: 3 } @media (hover: none) { .d { v: visible } } }
@keyframes k { 50% { opacity: .5; } }`)
	got := fmt.Sprint(rules)
	want := `[{[] a, b [[x 1] [y calc(2px + 4px)]]} {[@media (max-width: 599px)] .c [[z 3]]} {[@media (max-width: 599px) @media (hover: none)] .d [[v visible]]} {[@keyframes k] 50% [[opacity .5]]}]`
	if got != want {
		t.Errorf("parseCSS:\n got %s\nwant %s", got, want)
	}
}

// emojiProne are the code points a phone may draw as a colour emoji: the media-control
// glyphs, ▶ and ◀, the arrows the UI once used for links and modes, and the emoji planes.
func emojiProne(r rune) bool {
	switch {
	case r == 0x2197, r >= 0x23E9 && r <= 0x23FA, r == 0x25B6, r == 0x25C0, r == 0x27F3, r >= 0x2913 && r <= 0x292E, r >= 0x1F300:
		return true
	}
	return false
}

// Every icon is a Material Symbols path, and no text glyph stands in for one: the
// TypeScript draws only through icons.ts, and the markup it shares with the lookdev and
// index.html carries no emoji-prone character.
// Invariant: I-293
func TestIcons_MaterialSymbolsOnly(t *testing.T) {
	src := strings.ReplaceAll(readAll(t, filepath.Join(srcDir, "icons.ts")), "\r\n", "\n") // a Windows checkout ends lines with CRLF
	entries := regexp.MustCompile(`(?m)^ {2}(\w+): "([^"]*)",$`).FindAllStringSubmatch(src, -1)
	if len(entries) < 20 {
		t.Fatalf("icons.ts: %d entries, want the docs/13 Icons list", len(entries))
	}
	pathData := regexp.MustCompile(`^[Mm][0-9MmLlHhVvCcSsQqTtAaZz.,\- ]+$`)
	for _, e := range entries {
		if !pathData.MatchString(e[2]) {
			t.Errorf("icons.ts %s is not one path's data", e[1])
		}
	}
	if !strings.Contains(src, `viewBox: "0 -960 960 960"`) {
		t.Error(`icons.ts does not draw under the Material viewBox "0 -960 960 960"`)
	}
	for _, p := range glob(t, filepath.Join(srcDir, "*.ts")) {
		ts := readAll(t, p)
		if filepath.Base(p) != "icons.ts" && strings.Contains(ts, `class: "icon"`) {
			t.Errorf("%s draws its own icon; use icon() from icons.ts", p)
		}
	}
	markup := append(glob(t, filepath.Join(lookdevDir, "*.html")), staticHTML)
	files := append(glob(t, filepath.Join(srcDir, "*.ts")), markup...)
	for _, p := range files {
		for i, line := range strings.Split(readAll(t, p), "\n") {
			for _, r := range line {
				if emojiProne(r) {
					t.Errorf("%s:%d: %q is drawn as an emoji on some phones; use a Material symbol", p, i+1, r)
				}
			}
		}
	}
	for _, m := range regexp.MustCompile(`<svg class="icon[^"]*" viewBox="([^"]*)"`).FindAllStringSubmatch(readAll(t, markup...), -1) {
		if m[1] != "0 -960 960 960" {
			t.Errorf("an icon in the markup uses viewBox %q, not Material's", m[1])
		}
	}
}

func TestIcons_EmojiProne_Table(t *testing.T) {
	for _, c := range []struct {
		r    rune
		want bool
	}{{'▶', true}, {'⏸', true}, {'⏮', true}, {'⟳', true}, {'⤮', true}, {'↗', true}, {'🎵', true}, {'×', false}, {'▴', false}, {'▾', false}, {'·', false}, {'…', false}, {'→', false}} {
		if got := emojiProne(c.r); got != c.want {
			t.Errorf("emojiProne(%q) = %v, want %v", c.r, got, c.want)
		}
	}
}
