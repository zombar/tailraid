package webui

import (
	"html"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/zombar/tailraid/internal/meta"
)

// The markdown renderer of docs/13 §4 (I-209), in the manner of errol's: every line is
// HTML-escaped before any construct is recognised, so the output's tags are only the
// renderer's own and every attribute value is one it built. The fragment is a pure
// function of the source, the file's folder and the deleted root it lives in.

// mdMaxDepth bounds nesting (quotes in quotes, lists in lists): deeper is paragraph text.
const mdMaxDepth = 16

var (
	mdFence   = regexp.MustCompile("^( {0,3})(`{3,}|~{3,})[ \t]*([^`\\s]*)")
	mdHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	mdClose   = regexp.MustCompile(`[ \t]+#+$`)
	mdHR      = regexp.MustCompile(`^ {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	mdQuote   = regexp.MustCompile(`^ {0,3}&gt; ?`)
	mdItem    = regexp.MustCompile(`^( *)([-*+]|\d{1,9}[.)])( +|$)(.*)$`)
	mdTask    = regexp.MustCompile(`^\[([ xX])\] +`)
	mdSep     = regexp.MustCompile(`^ *\|? *:?-+:? *(?:\| *:?-+:? *)*\|? *$`)
	mdLang    = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,32}$`)
	mdLink    = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\(\s*([^()\s]*)(?:\s+&#34;[^\n]*?&#34;)?\s*\)`)
	mdAngle   = regexp.MustCompile(`&lt;((?:https?://|mailto:)\S*?)&gt;`)
	mdBare    = regexp.MustCompile(`https?://\S+`)
	mdStrong  = []*regexp.Regexp{regexp.MustCompile(`\*\*([^\s*](?:[^*\n]*?[^\s*])?)\*\*`), regexp.MustCompile(`(^|[^\pL\pN_])__([^\s_](?:[^_\n]*?[^\s_])?)__($|[^\pL\pN_])`)}
	mdEm      = []*regexp.Regexp{regexp.MustCompile(`\*([^\s*](?:[^*\n]*?[^\s*])?)\*`), regexp.MustCompile(`(^|[^\pL\pN_])_([^\s_](?:[^_\n]*?[^\s_])?)_($|[^\pL\pN_])`)}
	mdStrike  = regexp.MustCompile(`~~([^~\n]+)~~`)
)

type mdRenderer struct {
	out   strings.Builder
	dir   []string // the file's folder: volume parts, or parts inside the deleted root
	root  meta.Ino // the deleted root (0: live)
	ids   map[string]int
	depth int
}

// renderMarkdown renders src as an HTML fragment.
func renderMarkdown(src []byte, dir []string, root meta.Ino) string {
	s := strings.ToValidUTF8(string(src), "�")
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\x00", "�", "\t", "    ").Replace(s)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = html.EscapeString(lines[i])
	}
	m := &mdRenderer{dir: dir, root: root, ids: map[string]int{}}
	m.blocks(lines)
	return m.out.String()
}

func blank(l string) bool { return strings.TrimSpace(l) == "" }

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

func dedent(l string, n int) string { return l[min(indentOf(l), n):] }

// startsBlock reports a line that ends a paragraph: anything but plain text. Only a
// list that starts at 1 interrupts a paragraph, so "2019. was a year" stays text.
func startsBlock(l string) bool {
	if mdFence.MatchString(l) || mdHeading.MatchString(l) || mdHR.MatchString(l) || mdQuote.MatchString(l) {
		return true
	}
	if m := mdItem.FindStringSubmatch(l); len(m) == 5 && m[4] != "" {
		return !isOrdered(m[2]) || strings.HasPrefix(m[2], "1") && len(m[2]) == 2
	}
	return false
}

func isOrdered(marker string) bool { return marker[0] >= '0' && marker[0] <= '9' }

func (m *mdRenderer) blocks(ls []string) {
	m.depth++
	defer func() { m.depth-- }()
	for i := 0; i < len(ls); {
		l := ls[i]
		switch {
		case blank(l):
			i++
		case m.depth > mdMaxDepth:
			i = m.paragraph(ls, i)
		case mdFence.MatchString(l):
			i = m.fenced(ls, i)
		case strings.HasPrefix(l, "    "):
			i = m.indented(ls, i)
		case mdHeading.MatchString(l):
			m.heading(l)
			i++
		case mdHR.MatchString(l):
			m.out.WriteString("<hr>\n")
			i++
		case mdQuote.MatchString(l):
			var inner []string
			for i < len(ls) && mdQuote.MatchString(ls[i]) {
				inner = append(inner, mdQuote.ReplaceAllString(ls[i], ""))
				i++
			}
			m.out.WriteString("<blockquote>\n")
			m.blocks(inner)
			m.out.WriteString("</blockquote>\n")
		case mdItem.MatchString(l):
			i = m.list(ls, i)
		case strings.Contains(l, "|") && i+1 < len(ls) && mdSep.MatchString(ls[i+1]) && strings.Contains(ls[i+1], "-"):
			i = m.table(ls, i)
		default:
			i = m.paragraph(ls, i)
		}
	}
}

func (m *mdRenderer) fenced(ls []string, i int) int {
	f := mdFence.FindStringSubmatch(ls[i])
	indent, fence, lang := len(f[1]), f[2], f[3]
	closing := func(l string) bool { // up to three spaces, then at least as many of the fence's character
		t := strings.TrimLeft(l, " ")
		run := strings.TrimRight(t, " \t")
		return len(l)-len(t) <= 3 && len(run) >= len(fence) && strings.Trim(run, fence[:1]) == ""
	}
	var body []string
	i++
	for ; i < len(ls); i++ {
		if closing(ls[i]) {
			i++
			break
		}
		body = append(body, dedent(ls[i], indent))
	}
	m.code(body, lang)
	return i
}

func (m *mdRenderer) indented(ls []string, i int) int {
	var body []string
	for ; i < len(ls) && (blank(ls[i]) || strings.HasPrefix(ls[i], "    ")); i++ {
		body = append(body, dedent(ls[i], 4))
	}
	for len(body) > 0 && blank(body[len(body)-1]) {
		body = body[:len(body)-1]
	}
	m.code(body, "")
	return i
}

func (m *mdRenderer) code(body []string, lang string) {
	m.out.WriteString("<pre><code")
	if mdLang.MatchString(lang) {
		m.out.WriteString(` data-lang="` + lang + `"`) // [A-Za-z0-9_+#.-]: nothing to escape
	}
	m.out.WriteString(">" + strings.Join(body, "\n") + "</code></pre>\n")
}

func (m *mdRenderer) heading(l string) {
	h := mdHeading.FindStringSubmatch(l)
	level := min(len(h[1])+1, 6) // the file's # is the page's h2
	text := mdClose.ReplaceAllString(h[2], "")
	if strings.Trim(text, "#") == "" {
		text = ""
	}
	tag := "h" + strconv.Itoa(level)
	m.out.WriteString("<" + tag + ` id="` + html.EscapeString(m.slug(text)) + `">` + m.inline(text) + "</" + tag + ">\n")
}

// slug is a heading's id: its words lowercased and joined by hyphens, numbered when
// the same slug comes again.
func (m *mdRenderer) slug(text string) string {
	plain := html.UnescapeString(text)
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(plain) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		default:
			dash = true
		}
	}
	s := b.String()
	if s == "" {
		s = "section"
	}
	n := m.ids[s]
	m.ids[s] = n + 1
	if n > 0 {
		s += "-" + strconv.Itoa(n)
	}
	return s
}

func (m *mdRenderer) paragraph(ls []string, i int) int {
	var para []string
	for ; i < len(ls) && !blank(ls[i]) && (len(para) == 0 || !startsBlock(ls[i])); i++ {
		para = append(para, strings.TrimLeft(ls[i], " "))
	}
	m.out.WriteString("<p>" + m.lines(para) + "</p>\n")
	return i
}

// lines joins paragraph lines, a line ending in two spaces or a backslash a hard break.
func (m *mdRenderer) lines(ls []string) string {
	for j := range ls {
		l := ls[j]
		if j < len(ls)-1 && (strings.HasSuffix(l, "  ") || strings.HasSuffix(l, `\`)) {
			ls[j] = strings.TrimRight(strings.TrimSuffix(l, `\`), " ") + "\x00"
		} else {
			ls[j] = strings.TrimRight(l, " ")
		}
	}
	return strings.ReplaceAll(m.inline(strings.Join(ls, "\n")), "\x00", "<br>")
}

func (m *mdRenderer) list(ls []string, i int) int {
	first := mdItem.FindStringSubmatch(ls[i])
	indent, ordered := len(first[1]), isOrdered(first[2])
	tag := "ul"
	if ordered {
		tag = "ol"
		if n, _ := strconv.Atoi(strings.TrimRight(first[2], ".)")); n != 1 {
			m.out.WriteString(`<ol start="` + strconv.Itoa(n) + `">` + "\n")
		} else {
			m.out.WriteString("<ol>\n")
		}
	} else {
		m.out.WriteString("<ul>\n")
	}
	for i < len(ls) {
		it := mdItem.FindStringSubmatch(ls[i])
		if len(it) != 5 || len(it[1]) != indent || isOrdered(it[2]) != ordered {
			break
		}
		col := len(it[1]) + len(it[2]) + max(len(it[3]), 1)
		body := []string{it[4]}
		i++
		for i < len(ls) {
			l := ls[i]
			if blank(l) {
				j := i + 1
				for j < len(ls) && blank(ls[j]) {
					j++
				}
				if j < len(ls) && indentOf(ls[j]) > indent {
					for ; i < j; i++ {
						body = append(body, "")
					}
					continue
				}
				break
			}
			if indentOf(l) > indent {
				body = append(body, dedent(l, col))
				i++
				continue
			}
			if !startsBlock(l) && !mdItem.MatchString(l) && body[len(body)-1] != "" { // a lazy continuation line
				body = append(body, strings.TrimLeft(l, " "))
				i++
				continue
			}
			break
		}
		m.item(body)
		if i < len(ls) && blank(ls[i]) { // a blank line between two items of this list
			j := i + 1
			for j < len(ls) && blank(ls[j]) {
				j++
			}
			if j < len(ls) {
				if nx := mdItem.FindStringSubmatch(ls[j]); len(nx) == 5 && len(nx[1]) == indent && isOrdered(nx[2]) == ordered {
					i = j
				}
			}
		}
	}
	m.out.WriteString("</" + tag + ">\n")
	return i
}

func (m *mdRenderer) item(body []string) {
	lead := 0
	for lead < len(body) && !blank(body[lead]) && (lead == 0 || !startsBlock(body[lead])) {
		lead++
	}
	text := strings.Join(body[:lead], "\n")
	m.out.WriteString("<li")
	if t := mdTask.FindStringSubmatch(text); t != nil {
		m.out.WriteString(` class="task"><input type="checkbox"`)
		if t[1] != " " {
			m.out.WriteString(" checked")
		}
		m.out.WriteString(" disabled> ")
		text = text[len(t[0]):]
	} else {
		m.out.WriteString(">")
	}
	m.out.WriteString(m.lines(strings.Split(text, "\n")))
	if rest := body[lead:]; len(rest) > 0 {
		m.out.WriteString("\n")
		m.blocks(rest)
	}
	m.out.WriteString("</li>\n")
}

// cells splits a table row on its pipes; `\|` is a literal pipe.
func cells(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	if strings.HasSuffix(l, "|") && !strings.HasSuffix(l, `\|`) {
		l = l[:len(l)-1]
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(l); i++ {
		switch {
		case l[i] == '\\' && i+1 < len(l) && l[i+1] == '|':
			cur.WriteByte('|')
			i++
		case l[i] == '|':
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(l[i])
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

func (m *mdRenderer) table(ls []string, i int) int {
	head := cells(ls[i])
	var align []string
	for _, c := range cells(ls[i+1]) {
		switch l, r := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":"); {
		case l && r:
			align = append(align, ` align="center"`)
		case r:
			align = append(align, ` align="right"`)
		case l:
			align = append(align, ` align="left"`)
		default:
			align = append(align, "")
		}
	}
	row := func(cs []string, tag string) {
		m.out.WriteString("<tr>")
		for j := range head {
			c, a := "", ""
			if j < len(cs) {
				c = cs[j]
			}
			if j < len(align) {
				a = align[j]
			}
			m.out.WriteString("<" + tag + a + ">" + m.inline(c) + "</" + tag + ">")
		}
		m.out.WriteString("</tr>")
	}
	m.out.WriteString("<table>\n<thead>")
	row(head, "th")
	m.out.WriteString("</thead>\n<tbody>\n")
	i += 2
	for ; i < len(ls) && !blank(ls[i]) && strings.Contains(ls[i], "|"); i++ {
		row(cells(ls[i]), "td")
		m.out.WriteString("\n")
	}
	m.out.WriteString("</tbody>\n</table>\n")
	return i
}

// --- inline

// piece is a run of the inline text: escaped text still to scan, or finished HTML.
type piece struct {
	s    string
	done bool
}

// inline renders escaped text: code spans, then links and images, then autolinks,
// then emphasis on what is left.
func (m *mdRenderer) inline(s string) string {
	ps := codeSpans(s)
	ps = split(ps, mdLink, m.link)
	ps = split(ps, mdAngle, func(g []string) string { return m.autolink(g[1]) })
	ps = split(ps, mdBare, func(g []string) string { return m.bare(g[0]) })
	var b strings.Builder
	for _, p := range ps {
		if p.done {
			b.WriteString(p.s)
		} else {
			b.WriteString(emphasis(p.s))
		}
	}
	return b.String()
}

func codeSpans(s string) []piece {
	var out []piece
	for {
		i := strings.IndexByte(s, '`')
		if i < 0 {
			return append(out, piece{s: s})
		}
		n := i
		for n < len(s) && s[n] == '`' {
			n++
		}
		run := s[i:n]
		j := strings.Index(s[n:], run)
		for j >= 0 && n+j+len(run) < len(s) && s[n+j+len(run)] == '`' { // a longer run is not the closer
			k := strings.Index(s[n+j+len(run):], run)
			if k < 0 {
				j = -1
				break
			}
			j += len(run) + k
		}
		if j < 0 {
			out = append(out, piece{s: s[:n]})
			s = s[n:]
			continue
		}
		body := s[n : n+j]
		if t := strings.TrimSpace(body); t != "" {
			body = t
		}
		out = append(out, piece{s: s[:i]}, piece{s: "<code>" + body + "</code>", done: true})
		s = s[n+j+len(run):]
	}
}

func split(ps []piece, re *regexp.Regexp, render func([]string) string) []piece {
	var out []piece
	for _, p := range ps {
		if p.done {
			out = append(out, p)
			continue
		}
		last := 0
		for _, ix := range re.FindAllStringSubmatchIndex(p.s, -1) {
			g := make([]string, len(ix)/2)
			for k := range g {
				if ix[2*k] >= 0 {
					g[k] = p.s[ix[2*k]:ix[2*k+1]]
				}
			}
			frag := render(g)
			if frag == "" { // not ours: leave the text as it was
				continue
			}
			out = append(out, piece{s: p.s[last:ix[0]]}, piece{s: frag, done: true})
			last = ix[1]
		}
		out = append(out, piece{s: p.s[last:]})
	}
	return out
}

func emphasis(s string) string {
	s = mdStrong[0].ReplaceAllString(s, "<strong>$1</strong>")
	s = mdStrong[1].ReplaceAllString(s, "$1<strong>$2</strong>$3")
	s = mdStrike.ReplaceAllString(s, "<del>$1</del>")
	s = mdEm[0].ReplaceAllString(s, "<em>$1</em>")
	return mdEm[1].ReplaceAllString(s, "$1<em>$2</em>$3")
}

// link renders [text](url) and ![alt](src) by the policy of docs/13 §4: a link keeps an
// http, https, mailto or # target, a volume path becomes the file route or /files/;
// an image is a volume path or its alt text.
func (m *mdRenderer) link(g []string) string {
	image, text, raw := g[1] == "!", g[2], html.UnescapeString(g[3])
	if image {
		p, ok := m.volumePath(raw)
		if !ok {
			return text
		}
		return `<img alt="` + text + `" src="` + html.EscapeString(m.fileURL(p)) + `">`
	}
	label := emphasis(text)
	if label == "" {
		label = html.EscapeString(raw)
	}
	switch scheme(raw) {
	case "http", "https":
		return `<a href="` + html.EscapeString(raw) + `" rel="noopener noreferrer" target="_blank">` + label + "</a>"
	case "mailto":
		return `<a href="` + html.EscapeString(raw) + `">` + label + "</a>"
	case "":
	default:
		return text // any other scheme is text
	}
	if strings.HasPrefix(raw, "#") {
		return `<a href="` + html.EscapeString(raw) + `">` + label + "</a>"
	}
	p, ok := m.volumePath(raw)
	if !ok {
		return text
	}
	if e := strings.ToLower(path.Ext(p)); e == ".md" || e == ".markdown" {
		return `<a href="` + html.EscapeString(m.route(p)) + `">` + label + "</a>"
	}
	return `<a href="` + html.EscapeString(m.fileURL(p)) + `">` + label + "</a>"
}

func (m *mdRenderer) autolink(raw string) string {
	u := html.UnescapeString(raw)
	switch scheme(u) {
	case "http", "https":
		return `<a href="` + html.EscapeString(u) + `" rel="noopener noreferrer" target="_blank">` + raw + "</a>"
	case "mailto":
		return `<a href="` + html.EscapeString(u) + `">` + raw + "</a>"
	}
	return ""
}

// bare links a URL in running text, leaving trailing punctuation and quotes outside.
func (m *mdRenderer) bare(raw string) string {
	trail := ""
	for {
		switch {
		case strings.HasSuffix(raw, "&#39;"), strings.HasSuffix(raw, "&#34;"):
			trail = raw[len(raw)-5:] + trail
			raw = raw[:len(raw)-5]
			continue
		case strings.HasSuffix(raw, "&gt;"):
			trail = raw[len(raw)-4:] + trail
			raw = raw[:len(raw)-4]
			continue
		case raw != "" && strings.ContainsRune(".,;:!?)]*_~", rune(raw[len(raw)-1])):
			trail = raw[len(raw)-1:] + trail
			raw = raw[:len(raw)-1]
			continue
		}
		break
	}
	if len(raw) <= len("https://") {
		return ""
	}
	return m.autolink(raw) + trail
}

// scheme is a URL's scheme in lower case, "" for a path or an anchor.
func scheme(u string) string {
	i := strings.IndexAny(u, ":/?#")
	if i <= 0 || u[i] != ':' {
		return ""
	}
	return strings.ToLower(u[:i])
}

// volumePath resolves a link target against the file's folder: a volume path, never a
// URL with a scheme or a host.
func (m *mdRenderer) volumePath(raw string) (string, bool) {
	if raw == "" || scheme(raw) != "" || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "#") {
		return "", false
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if u, err := url.PathUnescape(raw); err == nil {
		raw = u
	}
	if raw == "" {
		return "", false
	}
	if strings.HasPrefix(raw, "/") {
		return path.Clean(raw), true
	}
	return path.Join("/"+strings.Join(m.dir, "/"), raw), true
}

func encodeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// route is the UI's file view of p (docs/13 §1); fileURL is /files/ (docs/13 §3).
func (m *mdRenderer) route(p string) string {
	if m.root != 0 {
		return "#/trashfile/" + strconv.FormatUint(uint64(m.root), 10) + encodeSegments(p)
	}
	return "#/file" + encodeSegments(p)
}

func (m *mdRenderer) fileURL(p string) string {
	u := "/files" + encodeSegments(p)
	if m.root != 0 {
		u += "?in=" + strconv.FormatUint(uint64(m.root), 10)
	}
	return u
}
