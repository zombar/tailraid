package webui_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/webui"
)

const ext = ` rel="noopener noreferrer" target="_blank"`

// One golden case per construct of docs/13 §4.
// Invariant: I-209
func TestMarkdown_Table(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"heading levels", "# Title\n## Sub\n###### Six", `<h2 id="title">Title</h2>` + "\n" + `<h3 id="sub">Sub</h3>` + "\n" + `<h6 id="six">Six</h6>` + "\n"},
		{"heading closing hashes and duplicate ids", "## A b ##\n## A b", `<h3 id="a-b">A b</h3>` + "\n" + `<h3 id="a-b-1">A b</h3>` + "\n"},
		{"paragraph", "one\ntwo\n\nthree", "<p>one\ntwo</p>\n<p>three</p>\n"},
		{"hard breaks", "a  \nb\\\nc", "<p>a<br>\nb<br>\nc</p>\n"},
		{"emphasis", "*em* _em_ **strong** __strong__ ~~del~~ `a < b`", "<p><em>em</em> <em>em</em> <strong>strong</strong> <strong>strong</strong> <del>del</del> <code>a &lt; b</code></p>\n"},
		{"snake case stays", "a snake_case_name and 2*3*4", "<p>a snake_case_name and 2<em>3</em>4</p>\n"},
		{"code span keeps stars", "`*not em*`", "<p><code>*not em*</code></p>\n"},
		{"fenced", "```go\nx := 1 < 2\n\n```", `<pre><code data-lang="go">x := 1 &lt; 2` + "\n" + `</code></pre>` + "\n"},
		{"fenced tilde, odd info string", "~~~ <x>\ny\n~~~", "<pre><code>y</code></pre>\n"},
		{"unclosed fence runs to the end", "```\na\nb", "<pre><code>a\nb</code></pre>\n"},
		{"indented", "    code\n      more\n\nafter", "<pre><code>code\n  more</code></pre>\n<p>after</p>\n"},
		{"quote", "> quote\n> more\n>\n> # in", "<blockquote>\n<p>quote\nmore</p>\n<h2 id=\"in\">in</h2>\n</blockquote>\n"},
		{"lists nested", "- a\n- b\n  - c\n  - d\n- e", "<ul>\n<li>a</li>\n<li>b\n<ul>\n<li>c</li>\n<li>d</li>\n</ul>\n</li>\n<li>e</li>\n</ul>\n"},
		{"ordered with start", "3. x\n4. y", "<ol start=\"3\">\n<li>x</li>\n<li>y</li>\n</ol>\n"},
		{"ordered from one, loose", "1. x\n\n2. y", "<ol>\n<li>x</li>\n<li>y</li>\n</ol>\n"},
		{"tasks", "- [ ] todo\n- [x] done", `<ul>` + "\n" + `<li class="task"><input type="checkbox" disabled> todo</li>` + "\n" + `<li class="task"><input type="checkbox" checked disabled> done</li>` + "\n</ul>\n"},
		{"number does not interrupt a paragraph", "In\n2019. it rained", "<p>In\n2019. it rained</p>\n"},
		{
			"table with alignment", "| a | b | c |\n|:--|--:|:-:|\n| 1 | 2 |\n| x \\| y | z | w | extra |",
			"<table>\n<thead><tr><th align=\"left\">a</th><th align=\"right\">b</th><th align=\"center\">c</th></tr></thead>\n<tbody>\n" +
				"<tr><td align=\"left\">1</td><td align=\"right\">2</td><td align=\"center\"></td></tr>\n" +
				"<tr><td align=\"left\">x | y</td><td align=\"right\">z</td><td align=\"center\">w</td></tr>\n</tbody>\n</table>\n",
		},
		{"thematic breaks", "---\n* * *\n___", "<hr>\n<hr>\n<hr>\n"},
		{"autolink", "see <https://x.org/a?b=1&c=2>.", `<p>see <a href="https://x.org/a?b=1&amp;c=2"` + ext + `>https://x.org/a?b=1&amp;c=2</a>.</p>` + "\n"},
		{"bare url keeps its underscores", "go to https://x.org/p_q_r.", `<p>go to <a href="https://x.org/p_q_r"` + ext + `>https://x.org/p_q_r</a>.</p>` + "\n"},
		{"crlf and tabs", "a\r\n\r\n\tcode", "<p>a</p>\n<pre><code>code</code></pre>\n"},
	} {
		if got := webui.RenderMarkdown([]byte(tc.in), []string{"docs"}, 0); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// A link keeps an http, https, mailto or # target; a volume path becomes the file route
// (.md) or /files/; an image is a volume path or its alt text; inside a deleted root
// both carry the root.
// Invariant: I-209
func TestMarkdown_LinksAndImages_Policy(t *testing.T) {
	for _, tc := range []struct {
		in   string
		root meta.Ino
		want string
	}{
		{"[t](other.md)", 0, `<a href="#/file/docs/other.md">t</a>`},
		{"[t](../notes/a%20b.markdown#x)", 0, `<a href="#/file/notes/a%20b.markdown">t</a>`},
		{"[t](/top.md)", 0, `<a href="#/file/top.md">t</a>`},
		{"[t](../../../../etc/x.md)", 0, `<a href="#/file/etc/x.md">t</a>`},
		{"[t](pic.png)", 0, `<a href="/files/docs/pic.png">t</a>`},
		{"[t](https://e.com/?a=1&b=2)", 0, `<a href="https://e.com/?a=1&amp;b=2"` + ext + `>t</a>`},
		{"[*t*](HTTP://E.COM)", 0, `<a href="HTTP://E.COM"` + ext + `><em>t</em></a>`},
		{"[t](mailto:a@b.c)", 0, `<a href="mailto:a@b.c">t</a>`},
		{"[t](#sec)", 0, `<a href="#sec">t</a>`},
		{"[t](javascript:void)", 0, `t`},
		{"[t](JavaScript:void)", 0, `t`},
		{"[t](data:text/html,x)", 0, `t`},
		{"[t](//evil.com/x.md)", 0, `t`},
		{"[](vbscript:x)", 0, `[](vbscript:x)`},
		{`[t](a.md "title")`, 0, `<a href="#/file/docs/a.md">t</a>`},
		{"![alt](img.png)", 0, `<img alt="alt" src="/files/docs/img.png">`},
		{"![alt](https://e.com/x.png)", 0, `alt`},
		{"![a&b](//e.com/x.png)", 0, `a&amp;b`},
		{"[t](a.md)", 42, `<a href="#/trashfile/42/docs/a.md">t</a>`},
		{"![a](b c.png)", 42, `![a](b c.png)`},
		{"![a](b%20c.png)", 42, `<img alt="a" src="/files/docs/b%20c.png?in=42">`},
		{"[t](x.pdf)", 42, `<a href="/files/docs/x.pdf?in=42">t</a>`},
	} {
		got := webui.RenderMarkdown([]byte(tc.in), []string{"docs"}, tc.root)
		if want := "<p>" + tc.want + "</p>\n"; got != want {
			t.Errorf("%s (root %d):\n got %q\nwant %q", tc.in, tc.root, got, want)
		}
	}
}

// Markup in the file is text: no tag, attribute or entity of the source survives.
// Invariant: I-209
func TestMarkdown_RawHTMLIsText(t *testing.T) {
	in := "<script>alert(1)</script>\n\n<div onclick=\"x\">hi &amp; bye</div>\n\n# <img src=x onerror=y>\n\n- <b>x</b>\n\n| <i>a</i> |\n|---|\n| &lt; |"
	want := "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>\n" +
		"<p>&lt;div onclick=&#34;x&#34;&gt;hi &amp;amp; bye&lt;/div&gt;</p>\n" +
		"<h2 id=\"img-src-x-onerror-y\">&lt;img src=x onerror=y&gt;</h2>\n" +
		"<ul>\n<li>&lt;b&gt;x&lt;/b&gt;</li>\n</ul>\n" +
		"<table>\n<thead><tr><th>&lt;i&gt;a&lt;/i&gt;</th></tr></thead>\n<tbody>\n<tr><td>&amp;lt;</td></tr>\n</tbody>\n</table>\n"
	if got := webui.RenderMarkdown([]byte(in), nil, 0); got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

var (
	ownTag    = regexp.MustCompile(`^</?(h[2-6]|p|em|strong|del|code|pre|blockquote|ul|ol|li|input|table|thead|tbody|tr|th|td|hr|br|a|img)[ >]`)
	ownEntity = regexp.MustCompile(`^&(amp|lt|gt|#34|#39);`)
	ownHref   = regexp.MustCompile(`href="([^"]*)"`)
	ownSrc    = regexp.MustCompile(`src="([^"]*)"`)
)

// Every < in the output opens one of the renderer's own tags, every & is one of its
// own entities, every href and src is one its policy allows, and the output is a pure
// function of the input.
// Invariant: I-209
func FuzzMarkdown(f *testing.F) {
	for _, s := range []string{"# a\n- [x] b\n> c", "<script>", "[x](javascript:y)", "| a |\n|---|\n| b |", "```\n<\n```", "![a](//x)", "**_~~`x`~~_**", strings.Repeat("> ", 100) + "deep", strings.Repeat("  - x\n", 40)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := webui.RenderMarkdown([]byte(s), []string{"d"}, 7)
		if again := webui.RenderMarkdown([]byte(s), []string{"d"}, 7); again != out {
			t.Fatal("not deterministic")
		}
		for i := range len(out) {
			switch out[i] {
			case '<':
				if !ownTag.MatchString(out[i:]) {
					t.Fatalf("foreign tag at %d in %q (from %q)", i, out, s)
				}
			case '&':
				if !ownEntity.MatchString(out[i:]) {
					t.Fatalf("foreign entity at %d in %q (from %q)", i, out, s)
				}
			}
		}
		for _, m := range ownHref.FindAllStringSubmatch(out, -1) {
			h := strings.ToLower(m[1])
			if !strings.HasPrefix(h, "#") && !strings.HasPrefix(h, "/files/") && !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") && !strings.HasPrefix(h, "mailto:") {
				t.Fatalf("href %q from %q", m[1], s)
			}
		}
		for _, m := range ownSrc.FindAllStringSubmatch(out, -1) {
			if !strings.HasPrefix(m[1], "/files/") {
				t.Fatalf("src %q from %q", m[1], s)
			}
		}
	})
}

// GET /api/render: .md and .markdown only (400 otherwise), at most TextMax bytes (413
// past it), text/html with the file's ETag and no-cache, 304 on a match; inside a
// deleted root the links carry it.
// Invariant: I-209
func TestWebUI_Render_Table(t *testing.T) {
	u := newUI(t)
	u.write("/docs/readme.md", 0, []byte("# Hi\n[b](b.md)"))
	u.write("/docs/notes.markdown", 0, []byte("*x*"))
	u.write("/docs/plain.txt", 0, []byte("# Hi"))
	for _, n := range []int{0, 1, webui.TextMax - 1, webui.TextMax, webui.TextMax + 1} {
		u.write(fmt.Sprintf("/size/%d.md", n), 0, []byte(strings.Repeat("a", n)))
	}
	u.flush()
	code, h, body := u.get("/api/render?path=/docs/readme.md")
	if code != 200 || h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cache-Control") != "no-cache" || h.Get("ETag") == "" {
		t.Fatalf("render: %d %v", code, h)
	}
	if string(body) != "<h2 id=\"hi\">Hi</h2>\n<p><a href=\"#/file/docs/b.md\">b</a></p>\n" {
		t.Fatalf("body %q", body)
	}
	if code, _, _ := u.get("/api/render?path=/docs/readme.md", "If-None-Match", h.Get("ETag")); code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", code)
	}
	if code, _, body := u.get("/api/render?path=/docs/notes.markdown"); code != 200 || string(body) != "<p><em>x</em></p>\n" {
		t.Fatalf(".markdown: %d %q", code, body)
	}
	for p, want := range map[string]int{"/docs/plain.txt": 400, "/docs": 400, "/docs/missing.md": 404} {
		if code, _, _ := u.get("/api/render?path=" + p); code != want {
			t.Errorf("%s: %d, want %d", p, code, want)
		}
	}
	for _, n := range []int{0, 1, webui.TextMax - 1, webui.TextMax, webui.TextMax + 1} {
		want := 200
		if n > webui.TextMax {
			want = http.StatusRequestEntityTooLarge
		}
		if code, _, _ := u.get(fmt.Sprintf("/api/render?path=/size/%d.md", n)); code != want {
			t.Errorf("%d bytes: %d, want %d", n, code, want)
		}
	}
	old, err := u.f.FSM.Resolve([]string{"docs"})
	must(t, err)
	u.del("/docs")
	code, _, body = u.get(fmt.Sprintf("/api/render?path=/readme.md&in=%d", old.Ino))
	if want := fmt.Sprintf("<h2 id=\"hi\">Hi</h2>\n<p><a href=\"#/trashfile/%d/b.md\">b</a></p>\n", old.Ino); code != 200 || string(body) != want {
		t.Fatalf("in the trash: %d %q, want %q", code, body, want)
	}
}

type fakeSearcher struct {
	q, under string
	limit    int
	hits     []search.Hit // what every search answers
}

func (f *fakeSearcher) Search(_ context.Context, q, under string, limit int) search.Result {
	f.q, f.under, f.limit = q, under, limit
	return search.Result{Query: q, Hits: append([]search.Hit{}, f.hits...)}
}

// GET /api/search passes the query and folder through, limit 50 by default and 400
// outside 1..200; without an index the route is 503.
// Invariant: I-208
func TestWebUI_Search_Table(t *testing.T) {
	fs := &fakeSearcher{}
	u := newUI(t, func(o *webui.Options) { o.Search = fs })
	for _, tc := range []struct {
		query string
		code  int
		limit int
	}{
		{"", 200, search.DefaultLimit},
		{"&limit=1", 200, 1},
		{fmt.Sprintf("&limit=%d", search.MaxLimit-1), 200, search.MaxLimit - 1},
		{fmt.Sprintf("&limit=%d", search.MaxLimit), 200, search.MaxLimit},
		{fmt.Sprintf("&limit=%d", search.MaxLimit+1), 400, 0},
		{"&limit=0", 400, 0},
		{"&limit=x", 400, 0},
	} {
		fs.limit = 0
		code, _, body := u.get("/api/search?q=agnes%20riv&under=/music" + tc.query)
		if code != tc.code || fs.limit != tc.limit {
			t.Errorf("%q: %d (limit %d), want %d (limit %d): %s", tc.query, code, fs.limit, tc.code, tc.limit, body)
		}
		if code == 200 && (fs.q != "agnes riv" || fs.under != "/music" || !strings.Contains(string(body), `"hits":[]`)) {
			t.Errorf("%q: passed %q %q, body %s", tc.query, fs.q, fs.under, body)
		}
	}
	if code, _, _ := newUI(t).get("/api/search?q=x"); code != http.StatusServiceUnavailable {
		t.Fatalf("no index: %d", code)
	}
}
