package webui_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
	"github.com/zombar/tailraid/internal/webui"
)

func (u *ui) statOf(path string) webui.Entry {
	u.t.Helper()
	code, _, body := u.get("/api/stat?path=" + path)
	if code != 200 {
		u.t.Fatalf("stat %s: %d %s", path, code, body)
	}
	var e webui.Entry
	must(u.t, json.Unmarshal(body, &e))
	return e
}

func (u *ui) put(path string, body []byte, hdr ...string) (int, http.Header) {
	u.t.Helper()
	resp := u.do(http.MethodPut, "/files"+path, bytes.NewReader(body), append([]string{webui.HeaderUI, "1"}, hdr...)...)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func (u *ui) mkdir(path string, hdr ...string) (int, string) {
	u.t.Helper()
	resp := u.do(http.MethodPost, "/api/mkdir", strings.NewReader(fmt.Sprintf(`{"path":%q}`, path)), hdr...)
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

// Invariant: I-71
func TestWebUI_CSRFHeaderRequired(t *testing.T) {
	u := newUI(t)
	// a PUT without the header is refused before the body is read
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, u.hs.URL+"/files/x", pr)
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
			t.Fatalf("PUT without header: %v", resp)
		}
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("server waited for the body before refusing")
	}
	if code, body := u.mkdir("/m"); code != 403 || !strings.Contains(body, webui.HeaderUI) {
		t.Fatalf("mkdir without header: %d %s", code, body)
	}
	if code, _ := u.mkdir("/m", webui.HeaderUI, "1"); code != 201 {
		t.Fatalf("mkdir with header: %d", code)
	}
	if code, _ := u.put("/x", []byte("x")); code != 201 {
		t.Fatalf("PUT with header: %d", code)
	}
	// no CORS anywhere: a preflight gets nothing to grant
	resp := u.do(http.MethodOptions, "/api/mkdir", nil, "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
	resp.Body.Close()
	for k := range resp.Header {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Fatalf("CORS header %s on a preflight", k)
		}
	}
	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		t.Fatalf("preflight answered %d", resp.StatusCode)
	}
}

// Invariant: I-70
func TestWebUI_Put_NewAndOverwrite(t *testing.T) {
	u := newUI(t)
	cs := int64(testharness.ChunkSize)
	body := testharness.Pattern(int(3*cs+5), 8)
	if code, _ := u.put("/p", body); code != 201 {
		t.Fatalf("new: %d", code)
	}
	st := u.statOf("/p")
	if st.Size != uint64(len(body)) || !st.Dirty {
		t.Fatalf("after PUT: %+v", st)
	}
	if got := u.readAt("/p", cs+3, 100); !bytes.Equal(got, body[cs+3:cs+103]) {
		t.Fatalf("vfs bytes at %d differ", cs+3)
	}
	if got := u.readAt("/p", 0, int64(len(body))); !bytes.Equal(got, body) {
		t.Fatalf("vfs bytes differ at %d", testharness.FirstDiff(got, body))
	}
	// a shorter overwrite truncates first
	short := testharness.Pattern(100, 9)
	if code, _ := u.put("/p", short); code != 204 {
		t.Fatalf("overwrite: %d", code)
	}
	if st := u.statOf("/p"); st.Size != 100 {
		t.Fatalf("after overwrite: %+v", st)
	}
	if got := u.readAt("/p", 0, 200); !bytes.Equal(got, short) {
		t.Fatalf("overwrite bytes differ")
	}
	// If-None-Match: * refuses to overwrite and changes nothing
	if code, _ := u.put("/p", []byte("nope"), "If-None-Match", "*"); code != http.StatusPreconditionFailed {
		t.Fatalf("If-None-Match: %d", code)
	}
	if got := u.readAt("/p", 0, 200); !bytes.Equal(got, short) {
		t.Fatalf("412 changed the bytes")
	}
	must(t, u.fs.MkdirAll("/dir", 0o755))
	if code, _ := u.put("/dir", []byte("x")); code != 400 {
		t.Fatalf("PUT on a directory: %d", code)
	}
	if code, _ := u.put("/", []byte("x")); code != 400 {
		t.Fatalf("PUT on the root: %d", code)
	}
	// after the flush the same bytes come back over HTTP too
	u.flush()
	if code, _, got := u.get("/files/p"); code != 200 || !bytes.Equal(got, short) {
		t.Fatalf("GET after flush: %d", code)
	}
}

// Invariant: I-70
func TestWebUI_Put_SizeBoundaries(t *testing.T) {
	u := newUI(t)
	cs := int(testharness.ChunkSize)
	for i, n := range []int{0, 1, cs - 1, cs, cs + 1} {
		name := fmt.Sprintf("/d/f%d", i) // parents are created
		body := testharness.Pattern(n, byte(i+1))
		if code, _ := u.put(name, body); code != 201 {
			t.Fatalf("%s (%d bytes): %d", name, n, code)
		}
		if st := u.statOf(name); st.Size != uint64(n) {
			t.Fatalf("%s: size %d, want %d", name, st.Size, n)
		}
		if got := u.readAt(name, 0, int64(n)+1); !bytes.Equal(got, body) {
			t.Fatalf("%s: bytes differ at %d", name, testharness.FirstDiff(got, body))
		}
	}
}

// Invariant: I-70
func TestWebUI_Put_WaitsForWALCap(t *testing.T) {
	cs := int64(testharness.ChunkSize)
	u := newUIWith(t, func(f *testharness.Fabric) { f.RestartOverlay(vfs.WithWALCap(2 * cs)) })
	body := testharness.Pattern(int(5*cs), 12)
	done := make(chan int, 1)
	go func() {
		code, _ := u.put("/big", body)
		done <- code
	}()
	// the upload stalls at the cap; flushing and letting its timer fire moves it on
	testharness.Eventually(t, func() bool {
		must(t, u.f.Flusher().FlushAll(ctx))
		u.f.Clk.Advance(250 * time.Millisecond)
		select {
		case code := <-done:
			if code != 201 {
				t.Fatalf("PUT: %d", code)
			}
			return true
		default:
			return false
		}
	}, "upload never completed past the WAL cap")
	u.flush()
	if got := u.readAt("/big", 0, 5*cs); !bytes.Equal(got, body) {
		t.Fatalf("bytes differ at %d", testharness.FirstDiff(got, body))
	}
}

// Invariant: I-70
func TestWebUI_Mkdir(t *testing.T) {
	u := newUI(t)
	hdr := []string{webui.HeaderUI, "1"}
	code, body := u.mkdir("/a/b", hdr...)
	if code != 201 || !strings.Contains(body, `"path":"/a/b"`) {
		t.Fatalf("mkdir: %d %s", code, body)
	}
	if st := u.statOf("/a/b"); st.Type != "dir" {
		t.Fatalf("stat: %+v", st)
	}
	if code, _ := u.mkdir("/a/b", hdr...); code != 409 {
		t.Fatalf("mkdir again: %d", code)
	}
	if code, _ := u.mkdir("/", hdr...); code != 400 {
		t.Fatalf("mkdir root: %d", code)
	}
	u.write("/f", 0, []byte("f"))
	if code, _ := u.mkdir("/f", hdr...); code != 409 {
		t.Fatalf("mkdir over a file: %d", code)
	}
	if code, _ := u.mkdir("/f/x", hdr...); code != 400 {
		t.Fatalf("mkdir through a file: %d", code)
	}
	resp := u.do(http.MethodPost, "/api/mkdir", strings.NewReader("not json"), hdr...)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad json: %d", resp.StatusCode)
	}
}

// Invariant: I-70
func TestWebUI_Mutations_StaleIs503(t *testing.T) {
	u := newUI(t)
	u.write("/old", 0, []byte("old"))
	u.flush()
	u.f.ProposeDown(errors.New("no leader"))
	u.f.SetStale(true)
	if code, h := u.put("/new", []byte("x")); code != 503 || h.Get("Retry-After") != "5" {
		t.Fatalf("PUT while stale: %d retry-after %q", code, h.Get("Retry-After"))
	}
	if code, _, _ := u.get("/api/stat?path=/new"); code != 404 {
		t.Fatalf("a refused PUT created something: %d", code)
	}
	if code, _ := u.mkdir("/m", webui.HeaderUI, "1"); code != 503 {
		t.Fatalf("mkdir while stale: %d", code)
	}
	if code, _, body := u.get("/files/old"); code != 200 || string(body) != "old" {
		t.Fatalf("reads must continue while stale: %d", code)
	}
}

// Invariant: I-73
func TestWebUI_StatusCached(t *testing.T) {
	u := newUI(t)
	for i := 0; i < 2; i++ {
		code, h, body := u.get("/api/status")
		if code != 200 || h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" || !strings.Contains(string(body), `"node":"one"`) {
			t.Fatalf("status: %d %v %s", code, h, body)
		}
	}
	if n := u.statusCalls.Load(); n != 1 {
		t.Fatalf("report built %d times for two polls inside the TTL", n)
	}
	u.f.Clk.Advance(2 * time.Second)
	u.get("/api/status")
	if n := u.statusCalls.Load(); n != 2 {
		t.Fatalf("report built %d times after the TTL", n)
	}
}

func TestWebUI_Chunks(t *testing.T) {
	u := newUI(t)
	cs := int(testharness.ChunkSize)
	u.write("/c", 0, testharness.Pattern(3*cs, 13))
	u.write("/empty", 0, nil)
	u.flush()
	type page struct {
		Total  int                 `json:"total"`
		Chunks []admin.ChunkReport `json:"chunks"`
	}
	fetch := func(q string) (int, page) {
		t.Helper()
		code, _, body := u.get("/api/chunks?" + q)
		var p page
		if code == 200 {
			must(t, json.Unmarshal(body, &p))
		}
		return code, p
	}
	if code, p := fetch("path=/c"); code != 200 || p.Total != 3 || len(p.Chunks) != 3 || !p.Chunks[0].Found {
		t.Fatalf("all: %d %+v", code, p)
	}
	if code, p := fetch("path=/c&limit=2"); code != 200 || p.Total != 3 || len(p.Chunks) != 2 {
		t.Fatalf("limit: %d %+v", code, p)
	}
	if code, p := fetch("path=/c&offset=2&limit=2"); code != 200 || len(p.Chunks) != 1 {
		t.Fatalf("offset: %d %+v", code, p)
	}
	if code, p := fetch("path=/c&limit=100000"); code != 200 || len(p.Chunks) != 3 {
		t.Fatalf("clamped limit: %d %+v", code, p)
	}
	if code, p := fetch("path=/empty"); code != 200 || p.Total != 0 || len(p.Chunks) != 0 {
		t.Fatalf("empty: %d %+v", code, p)
	}
	if code, _ := fetch("path=/"); code != 400 {
		t.Fatalf("dir: %d", code)
	}
	if code, _ := fetch("path=/nope"); code != 404 {
		t.Fatalf("missing: %d", code)
	}
}
