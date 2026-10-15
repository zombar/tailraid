package webui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/tailscale"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
	"github.com/zombar/tailraid/internal/webui"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"), goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"))
}

var (
	ctx  = context.Background()
	loop = netip.MustParseAddr("127.0.0.1") // httptest clients come from loopback
)

const (
	login   = "jim@example.com"
	stableS = "self-1"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ui serves a web UI over a Fabric with a real WhoIs authorizer; the loopback caller is
// member 1 while member is set and a plain tailnet device otherwise.
type ui struct {
	t           *testing.T
	f           *testharness.Fabric
	fs          *vfs.FS
	ts          *tailscale.Fake
	auth        *rpc.WhoIsAuthorizer
	srv         *webui.Server
	hs          *httptest.Server
	member      atomic.Bool
	statusCalls atomic.Int32
	client      *http.Client
}

func newUI(t *testing.T, opts ...func(*webui.Options)) *ui { return newUIWith(t, nil, opts...) }

// newUIWith runs prep on the fabric (overlay options) before the filesystem is built.
func newUIWith(t *testing.T, prep func(*testharness.Fabric), opts ...func(*webui.Options)) *ui {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	if prep != nil {
		prep(f)
	}
	return newUIOn(t, f, opts...)
}

// newUIOn serves a second web UI over an existing fabric: another node's page over the
// same FSM (docs/21 §6: a session is the volume's).
func newUIOn(t *testing.T, f *testharness.Fabric, opts ...func(*webui.Options)) *ui {
	t.Helper()
	u := &ui{t: t, f: f, fs: f.FS(), client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	u.ts = tailscale.NewFake(loop, stableS, nil)
	u.ts.SetUser(loop, login)
	u.member.Store(true)
	u.auth = &rpc.WhoIsAuthorizer{TS: u.ts, Clk: f.Clk, Member: func(addr string) (meta.Node, bool) {
		if u.member.Load() && addr == loop.String() {
			return meta.Node{Idx: 1, ID: "n1", Name: "one", Kind: meta.KindDevice, TSStableID: stableS, Addr: addr}, true
		}
		return meta.Node{}, false
	}}
	o := webui.Options{
		FS: u.fs, Store: f.FSM, Auth: u.auth, Login: login, Clk: f.Clk, Logger: slog.New(slog.DiscardHandler),
		Status: func() any { u.statusCalls.Add(1); return map[string]any{"node": "one"} },
		Chunks: func(ctx context.Context, p string, off, lim int) (int, []admin.ChunkReport, error) {
			return admin.DescribeChunksPage(ctx, f.FSM, nil, f, nil, p, off, lim)
		},
	}
	for _, opt := range opts {
		opt(&o)
	}
	srv, err := webui.New(o)
	must(t, err)
	u.srv = srv
	u.hs = httptest.NewServer(srv.Handler())
	t.Cleanup(u.hs.Close)
	return u
}

// do sends a request; hdr are alternating header names and values. The caller closes the body.
func (u *ui) do(method, path string, body io.Reader, hdr ...string) *http.Response {
	u.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, u.hs.URL+path, body)
	must(u.t, err)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := u.client.Do(req)
	must(u.t, err)
	return resp
}

func (u *ui) get(path string, hdr ...string) (int, http.Header, []byte) {
	u.t.Helper()
	resp := u.do(http.MethodGet, path, nil, hdr...)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	must(u.t, err)
	return resp.StatusCode, resp.Header, b
}

// write puts data at off through the vfs, creating the file (not its parents).
func (u *ui) write(name string, off int64, data []byte) {
	u.t.Helper()
	fh, err := u.fs.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	must(u.t, err)
	_, err = fh.Seek(off, io.SeekStart)
	must(u.t, err)
	_, err = fh.Write(data)
	must(u.t, err)
	must(u.t, fh.Close())
}

func (u *ui) flush() { u.t.Helper(); must(u.t, u.f.Flusher().FlushAll(ctx)) }

// readAt reads n bytes at off through the vfs, the oracle for every HTTP read.
func (u *ui) readAt(name string, off, n int64) []byte {
	u.t.Helper()
	fh, err := u.fs.Open(name)
	must(u.t, err)
	defer fh.Close()
	buf := make([]byte, n)
	k, err := fh.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		u.t.Fatal(err)
	}
	return buf[:k]
}

// Invariant: I-73
func TestWebUI_New_RequiresAuthorizer(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	if _, err := webui.New(webui.Options{FS: f.FS(), Store: f.FSM}); err == nil {
		t.Fatal("New without an authorizer must fail")
	}
	if _, err := webui.New(webui.Options{FS: f.FS(), Store: f.FSM, Auth: rpc.AllowAll{}}); err != nil {
		t.Fatal(err)
	}
}

// Invariant: I-68
func TestWebUI_Authorize_MemberOrSameUser(t *testing.T) {
	u := newUI(t)
	noLogin := newUI(t, func(o *webui.Options) { o.Login = "" })
	cases := []struct {
		name   string
		u      *ui
		member bool
		setup  func(*ui)
		want   int
	}{
		{"member, same login", u, true, nil, 200},
		{"member, other login", u, true, func(x *ui) { x.ts.SetUser(loop, "other@example.com") }, 200},
		{"non-member, same login", u, false, func(x *ui) { x.ts.SetUser(loop, login) }, 200},
		{"non-member, other login", u, false, func(x *ui) { x.ts.SetUser(loop, "other@example.com") }, 403},
		{"non-member, tagged with the right login", u, false, func(x *ui) { x.ts.SetUser(loop, login, "tag:server") }, 403},
		{"own login unresolved, caller has none", noLogin, false, func(x *ui) { x.ts.SetUser(loop, "") }, 403},
		{"own login unresolved, caller is a member", noLogin, true, nil, 200},
		{"not on the tailnet", u, false, func(x *ui) { x.ts.Remove(loop) }, 403},
		{"whois down, non-member", u, false, func(x *ui) { x.ts.Add(loop, stableS, "self"); x.ts.Err = errors.New("down") }, 503},
		{"whois down, member", u, true, func(x *ui) { x.ts.Err = errors.New("down") }, 503},
	}
	for _, c := range cases {
		c.u.member.Store(c.member)
		c.u.ts.Err = nil
		if c.setup != nil {
			c.setup(c.u)
		}
		c.u.auth.Purge()
		code, _, body := c.u.get("/api/ls?path=/")
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
		}
	}
}

// Invariant: I-68
func TestWebUI_Authorize_BeforeBody(t *testing.T) {
	u := newUI(t)
	u.member.Store(false)
	u.ts.SetUser(loop, "other@example.com")
	u.auth.Purge()
	// a huge body from a refused source: the server answers without reading it
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, u.hs.URL+"/files/x", pr)
	req.Header.Set(webui.HeaderUI, "1")
	req.ContentLength = 1 << 40
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := u.client.Do(req)
		if err != nil {
			done <- nil
			return
		}
		done <- resp
	}()
	select {
	case resp := <-done:
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("resp %v", resp)
		}
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("server waited for the body before refusing")
	}
}

// Invariant: I-72
func TestWebUI_Paths(t *testing.T) {
	u := newUI(t)
	must(t, u.fs.MkdirAll("/a", 0o755))
	must(t, u.fs.MkdirAll("/dir", 0o755))
	u.write("/a/b", 0, []byte("payload"))
	u.write("/file", 0, []byte("f"))
	u.flush()
	cases := []struct {
		path string
		want int
		body string // substring of the body, or empty for any
	}{
		{"/files/", 400, "is a directory"},
		{"/files/../a/b", 307, "Redirect"}, // the mux cleans and redirects, never content
		{"/files/a//b", 307, "Redirect"},
		{"/files/a%5Cb", 200, "payload"}, // backslash separator
		{"/files/dir", 400, "is a directory"},
		{"/files/missing", 404, ""},
		{"/files/a/b/c", 400, "not a directory"}, // through a file
		{"/api/ls?path=/file", 400, "not a directory"},
		{"/api/ls?path=", 200, `"path":"/"`},
		{"/api/ls?path=/a/../", 200, `"path":"/"`},
		{"/api/ls?path=%5Ca%5C", 200, `"path":"/a"`},
		{"/api/ls?path=/a/../../../a", 200, `"path":"/a"`},
		{"/api/stat?path=/nope", 404, ""},
		{"/api/stat?path=/", 200, `"type":"dir"`},
	}
	for _, c := range cases {
		code, _, body := u.get(c.path)
		if code != c.want || !strings.Contains(string(body), c.body) {
			t.Errorf("GET %s: %d %q, want %d containing %q", c.path, code, body, c.want, c.body)
		}
	}
}

type lsReply struct {
	Path    string        `json:"path"`
	Entries []webui.Entry `json:"entries"`
}

func TestWebUI_Ls_Stat(t *testing.T) {
	u := newUI(t)
	must(t, u.fs.MkdirAll("/d/sub", 0o755))
	u.write("/d/f", 0, testharness.Pattern(100, 1))
	u.flush()
	must(t, u.fs.Symlink("f", "/d/l"))
	u.write("/d/g", 10, testharness.Pattern(50, 2)) // dirty: not flushed
	code, _, body := u.get("/api/ls?path=/d")
	var ls lsReply
	must(t, json.Unmarshal(body, &ls))
	if code != 200 || ls.Path != "/d" || len(ls.Entries) != 4 {
		t.Fatalf("ls /d: %d %s", code, body)
	}
	byName := map[string]webui.Entry{}
	var order []string
	for _, e := range ls.Entries {
		byName[e.Name] = e
		order = append(order, e.Name)
	}
	if strings.Join(order, ",") != "f,g,l,sub" {
		t.Fatalf("order %v", order)
	}
	if e := byName["f"]; e.Type != "file" || e.Size != 100 || e.Dirty || e.Ino == 0 || e.Mode != 0o644 || e.Mtime.IsZero() {
		t.Fatalf("f %+v", e)
	}
	if e := byName["g"]; e.Type != "file" || e.Size != 60 || !e.Dirty {
		t.Fatalf("g %+v", e)
	}
	if e := byName["l"]; e.Type != "symlink" || e.Target != "f" {
		t.Fatalf("l %+v", e)
	}
	if e := byName["sub"]; e.Type != "dir" || e.Mode != 0o755 {
		t.Fatalf("sub %+v", e)
	}
	code, _, body = u.get("/api/stat?path=/d/f")
	var st webui.Entry
	must(t, json.Unmarshal(body, &st))
	if code != 200 || st.Path != "/d/f" || st.Name != "f" || st.Size != 100 || st.Ino != byName["f"].Ino {
		t.Fatalf("stat: %d %s", code, body)
	}
}

// Invariant: I-69
func TestWebUI_Get_RangeBoundaries(t *testing.T) {
	u := newUI(t)
	const cs = int64(testharness.ChunkSize)
	size := 3*cs + 7
	u.write("/r", 4096, testharness.Pattern(int(size-4096), 3)) // non-zero offset: a zero prefix
	u.flush()
	whole := u.readAt("/r", 0, size)
	if int64(len(whole)) != size {
		t.Fatalf("oracle size %d", len(whole))
	}
	for _, start := range []int64{0, 1, size - 1, size, size + 1} {
		code, h, body := u.get("/files/r", "Range", fmt.Sprintf("bytes=%d-", start))
		if start >= size {
			if code != http.StatusRequestedRangeNotSatisfiable || h.Get("Content-Range") != fmt.Sprintf("bytes */%d", size) {
				t.Errorf("start %d: %d %q", start, code, h.Get("Content-Range"))
			}
			continue
		}
		want := fmt.Sprintf("bytes %d-%d/%d", start, size-1, size)
		if code != http.StatusPartialContent || h.Get("Content-Range") != want || h.Get("Accept-Ranges") != "bytes" {
			t.Errorf("start %d: %d %q accept=%q", start, code, h.Get("Content-Range"), h.Get("Accept-Ranges"))
		}
		if !bytes.Equal(body, whole[start:]) {
			t.Errorf("start %d: body differs from the vfs at byte %d", start, testharness.FirstDiff(body, whole[start:]))
		}
	}
	// a range crossing a chunk boundary, and a suffix range
	lo, hi := cs-5, cs+9
	if code, h, body := u.get("/files/r", "Range", fmt.Sprintf("bytes=%d-%d", lo, hi)); code != 206 || h.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", lo, hi, size) || !bytes.Equal(body, whole[lo:hi+1]) {
		t.Errorf("chunk-crossing range: %d %q", code, h.Get("Content-Range"))
	}
	if code, _, body := u.get("/files/r", "Range", "bytes=-5"); code != 206 || !bytes.Equal(body, whole[size-5:]) {
		t.Errorf("suffix range: %d %q", code, body)
	}
	// no range: the whole file
	if code, h, body := u.get("/files/r"); code != 200 || h.Get("Content-Length") != fmt.Sprint(size) || h.Get("Accept-Ranges") != "bytes" || !bytes.Equal(body, whole) {
		t.Errorf("whole: %d len=%s", code, h.Get("Content-Length"))
	}
}

// Invariant: I-69
func TestWebUI_Get_ETagAndHead(t *testing.T) {
	u := newUI(t)
	u.write("/e", 100, testharness.Pattern(300, 4))
	u.flush()
	etagRe := regexp.MustCompile(`^"\d+\.\d+\.\d+"$`)
	code, h, _ := u.get("/files/e")
	etag := h.Get("ETag")
	if code != 200 || !etagRe.MatchString(etag) {
		t.Fatalf("GET: %d etag %q", code, etag)
	}
	if code, _, _ := u.get("/files/e", "If-None-Match", etag); code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", code)
	}
	resp := u.do(http.MethodHead, "/files/e", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("ETag") != etag || resp.Header.Get("Content-Length") != "400" || len(body) != 0 || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("HEAD: %d %v len=%d", resp.StatusCode, resp.Header, len(body))
	}
	// an overwrite that is flushed changes the ETag
	u.write("/e", 100, testharness.Pattern(300, 5))
	u.flush()
	code, h, _ = u.get("/files/e")
	if code != 200 || h.Get("ETag") == etag || !etagRe.MatchString(h.Get("ETag")) {
		t.Fatalf("after overwrite: %d etag %q (was %q)", code, h.Get("ETag"), etag)
	}
	etag = h.Get("ETag")
	// a dirty file has no ETag, and the stale validator never yields a 304
	u.write("/e", 100, testharness.Pattern(300, 6))
	code, h, body = u.get("/files/e", "If-None-Match", etag)
	if code != 200 || h.Get("ETag") != "" || !bytes.Equal(body[100:], testharness.Pattern(300, 6)) {
		t.Fatalf("dirty: %d etag %q", code, h.Get("ETag"))
	}
}

// Invariant: I-69
func TestWebUI_Get_DirtyOverlayReadYourWrites(t *testing.T) {
	u := newUI(t)
	data := testharness.Pattern(1000, 7)
	u.write("/w", 70000, data) // unflushed: only the overlay has it
	code, h, body := u.get("/files/w")
	if code != 200 || h.Get("Content-Length") != "71000" || len(body) != 71000 || !bytes.Equal(body[70000:], data) {
		t.Fatalf("dirty GET: %d len=%s", code, h.Get("Content-Length"))
	}
	if code, _, body := u.get("/files/w", "Range", "bytes=70000-70009"); code != 206 || !bytes.Equal(body, data[:10]) {
		t.Fatalf("dirty range: %d %q", code, body)
	}
}

// Invariant: I-69, I-71
func TestWebUI_Get_ContentTypes(t *testing.T) {
	u := newUI(t)
	cases := []struct{ name, typ string }{
		{"a.mkv", "video/x-matroska"},
		{"a.mp4", "video/mp4"},
		{"a.m4a", "audio/mp4"},
		{"a.flac", "audio/flac"},
		{"a.txt", "text/plain; charset=utf-8"},
		{"a.md", "text/markdown; charset=utf-8"},
		{"a.json", "application/json"},
		{"a.html", "text/html; charset=utf-8"},
		{"a.svg", "image/svg+xml"},
		{"a.bin", "application/octet-stream"},
		{"noext", "application/octet-stream"},
		{"a.ts", "application/octet-stream"},
	}
	for _, c := range cases {
		u.write("/"+c.name, 0, []byte("x"))
	}
	u.flush()
	for _, c := range cases {
		code, h, _ := u.get("/files/" + c.name)
		if code != 200 || h.Get("Content-Type") != c.typ {
			t.Errorf("%s: %d %q, want %q", c.name, code, h.Get("Content-Type"), c.typ)
		}
		if h.Get("Content-Security-Policy") != "sandbox" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: csp %q nosniff %q", c.name, h.Get("Content-Security-Policy"), h.Get("X-Content-Type-Options"))
		}
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header present", c.name)
		}
	}
	if _, h, _ := u.get("/files/a.txt?download=1"); !strings.HasPrefix(h.Get("Content-Disposition"), "attachment; filename=") || !strings.Contains(h.Get("Content-Disposition"), "a.txt") {
		t.Errorf("download: %q", h.Get("Content-Disposition"))
	}
}

// Invariant: I-73
func TestWebUI_StaticAssets(t *testing.T) {
	u := newUI(t)
	code, h, body := u.get("/")
	if code != 200 || h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cache-Control") != "no-cache" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
		!strings.Contains(string(body), "/assets/app.js") || !strings.Contains(string(body), "/assets/app.css") {
		t.Fatalf("index: %d %v", code, h)
	}
	if code, h, body := u.get("/assets/app.css"); code != 200 || h.Get("Content-Type") != "text/css; charset=utf-8" || !strings.Contains(string(body), "--accent") {
		t.Fatalf("app.css: %d %q", code, h.Get("Content-Type"))
	}
	if code, h, body := u.get("/assets/app.js"); code != 200 || h.Get("Content-Type") != "text/javascript; charset=utf-8" || len(body) == 0 {
		t.Fatalf("app.js: %d %q len=%d", code, h.Get("Content-Type"), len(body))
	}
	for _, p := range []string{"/assets/nope", "/assets/", "/index.html", "/nope"} {
		if code, _, _ := u.get(p); code != 404 {
			t.Errorf("%s: %d, want 404", p, code)
		}
	}
}
