package webui_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The browser UI has no CSS or HTML linter (none exists as a Go module), so this test is
// the lint docs/13 §6 asks for: src/app.css is the one stylesheet, the lookdev pages are
// the reference rendering and link it directly, and the TypeScript emits class names.
// A class the CSS defines that nothing uses is dead CSS; a class the TypeScript emits
// that neither the CSS nor the lookdev knows is drift between the app and its lookdev.

var (
	srcDir     = "src"
	staticHTML = "static/index.html"
	lookdevDir = filepath.Join("..", "..", "docs", "assets", "lookdev")
)

func readAll(t *testing.T, paths ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func glob(t *testing.T, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(pattern)
	if err != nil || len(m) == 0 {
		t.Fatalf("glob %s: %v (%d files)", pattern, err, len(m))
	}
	return m
}

// cssClasses returns every class name used in a selector of css (text outside braces and
// comments).
func cssClasses(css string) map[string]bool {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var sel strings.Builder
	depth := 0
	for _, r := range css {
		switch {
		case r == '{':
			depth++
		case r == '}':
			depth--
		case depth == 0:
			sel.WriteRune(r)
		}
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([A-Za-z_][\w-]*)`).FindAllStringSubmatch(sel.String(), -1) {
		out[m[1]] = true
	}
	return out
}

// markupClasses returns every word of every class="…" attribute in html.
func markupClasses(html string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		for w := range strings.FieldsSeq(m[1]) {
			out[w] = true
		}
	}
	return out
}

// tsClasses returns the class words the TypeScript emits through h(…, { class: "…" }),
// className assignments and classList calls, and the literal words it concatenates onto
// them ("toast" + " " + cls): every quoted string in those expressions.
func tsClasses(ts string) map[string]bool {
	out := map[string]bool{}
	pats := []*regexp.Regexp{
		regexp.MustCompile(`class:\s*("[^"]*"(?:\s*\+\s*\(?[^,}\n]*)?)`),
		regexp.MustCompile(`className\s*=\s*([^;\n]+)`),
		regexp.MustCompile(`classList\.(?:add|remove|toggle)\(([^)]*)\)`),
	}
	quoted := regexp.MustCompile(`"([^"]*)"`)
	compared := regexp.MustCompile(`[=!]==\s*"[^"]*"`) // j.state === "failed": a value, not a class
	for _, p := range pats {
		for _, m := range p.FindAllStringSubmatch(ts, -1) {
			for _, q := range quoted.FindAllStringSubmatch(compared.ReplaceAllString(m[1], ""), -1) {
				for w := range strings.FieldsSeq(q[1]) {
					out[w] = true
				}
			}
		}
	}
	return out
}

func TestStatic_ClassesAgreeAcrossCSSLookdevAndTypeScript(t *testing.T) {
	css := cssClasses(readAll(t, filepath.Join(srcDir, "app.css")))
	markup := markupClasses(readAll(t, append(glob(t, filepath.Join(lookdevDir, "*.html")), staticHTML)...))
	ts := tsClasses(readAll(t, glob(t, filepath.Join(srcDir, "*.ts"))...))

	// Classes the TypeScript builds from data at runtime (state names, chart series) are
	// listed here with the expression that produces them, so a reviewer sees them.
	dynamic := map[string]string{}

	var dead, drift []string
	for c := range css {
		if !markup[c] && !ts[c] && dynamic[c] == "" {
			dead = append(dead, c)
		}
	}
	for c := range ts {
		if !css[c] && !markup[c] {
			drift = append(drift, c)
		}
	}
	sort.Strings(dead)
	sort.Strings(drift)
	if len(dead) > 0 {
		t.Errorf("app.css defines classes nothing uses (dead CSS, or add the producing expression to dynamic): %v", dead)
	}
	if len(drift) > 0 {
		t.Errorf("the TypeScript emits classes app.css and the lookdev do not know (docs/13: edit app.css and the lookdev together): %v", drift)
	}
	t.Logf("%d css classes, %d in markup, %d in TypeScript", len(css), len(markup), len(ts))
}

// Every page has one title, a charset and a viewport, and index.html asks the Go handler
// only for assets it serves (webui/static.go).
func TestStatic_PagesAreWellFormedShells(t *testing.T) {
	pages := append(glob(t, filepath.Join(lookdevDir, "*.html")), staticHTML)
	for _, p := range pages {
		html := readAll(t, p)
		head := regexp.MustCompile(`(?s)<head>.*?</head>`).FindString(html)
		if n := strings.Count(head, "<title>"); n != 1 { // SVG tooltips in the body are <title> too
			t.Errorf("%s: %d <title> elements in <head>, want 1", p, n)
		}
		if !strings.Contains(html, `<meta charset=`) {
			t.Errorf("%s: no <meta charset>", p)
		}
		if !strings.Contains(html, `name="viewport"`) {
			t.Errorf("%s: no viewport meta", p)
		}
	}
	served := map[string]bool{"/assets/app.js": true, "/assets/app.css": true}
	html := readAll(t, staticHTML)
	for _, m := range regexp.MustCompile(`(?:src|href)="(/[^"]*)"`).FindAllStringSubmatch(html, -1) {
		if !served[m[1]] {
			t.Errorf("index.html references %s, which the handler does not serve", m[1])
		}
	}
}
