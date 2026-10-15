package webui_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// rawPut opens a connection and sends a PUT's headers (Content-Length: length) followed
// by data, which may be shorter than length: what a browser that is still sending, or
// one that was cut off, looks like on the wire. The caller finishes or drops the body.
func (u *ui) rawPut(path string, length int, data []byte, hdr ...string) net.Conn {
	u.t.Helper()
	conn, err := net.Dial("tcp", u.hs.Listener.Addr().String())
	must(u.t, err)
	var b strings.Builder
	fmt.Fprintf(&b, "PUT /files%s HTTP/1.1\r\nHost: tailraid\r\n%s: 1\r\nContent-Length: %d\r\n", path, webui.HeaderUI, length)
	for i := 0; i+1 < len(hdr); i += 2 {
		fmt.Fprintf(&b, "%s: %s\r\n", hdr[i], hdr[i+1])
	}
	b.WriteString("\r\n")
	_, err = conn.Write([]byte(b.String()))
	must(u.t, err)
	if len(data) > 0 {
		_, err = conn.Write(data)
		must(u.t, err)
	}
	return conn
}

// reply reads the response on a raw connection: the status and the decoded JSON body.
func (u *ui) reply(conn net.Conn) (int, http.Header, map[string]any) {
	u.t.Helper()
	must(u.t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	must(u.t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, resp.Header, out
}

// putCut sends data as the first bytes of a body announced as length bytes, then
// half-closes the connection as a dropped client does, and returns the server's reply.
func (u *ui) putCut(path string, length int, data []byte, hdr ...string) (int, map[string]any) {
	u.t.Helper()
	conn := u.rawPut(path, length, data, hdr...)
	defer conn.Close()
	must(u.t, conn.(*net.TCPConn).CloseWrite())
	code, _, body := u.reply(conn)
	return code, body
}

// putPart sends one part with Content-Range through the normal client.
func (u *ui) putPart(path string, data []byte, start, total int) (int, map[string]any) {
	u.t.Helper()
	resp := u.do(http.MethodPut, "/files"+path, bytes.NewReader(data), webui.HeaderUI, "1", "Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+len(data)-1, total))
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (u *ui) exists(path string) bool {
	u.t.Helper()
	_, err := u.fs.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		u.t.Fatal(err)
	}
	return err == nil
}

// An overwrite whose body is cut off leaves the old file untouched and no temp behind;
// a new file whose body is cut off is never created.
// Invariant: I-81
func TestWebUI_Put_AtomicOverwrite_DisconnectKeepsOld(t *testing.T) {
	u := newUI(t)
	old := testharness.Pattern(5000, 3)
	if code, _ := u.put("/a", old); code != 201 {
		t.Fatalf("first put: %d", code)
	}
	fresh := testharness.Pattern(20000, 4)
	code, body := u.putCut("/a", len(fresh), fresh[:7000])
	if code != 400 {
		t.Fatalf("cut-off overwrite: %d %v", code, body)
	}
	if got := u.readAt("/a", 0, 6000); !bytes.Equal(got, old) {
		t.Fatalf("old content changed (len %d)", len(got))
	}
	if u.exists("/.a.tailraid-upload") {
		t.Fatal("temp left behind")
	}
	if code, _ := u.putCut("/b", 1000, fresh[:100]); code != 400 {
		t.Fatalf("cut-off new file: %d", code)
	}
	if u.exists("/b") || u.exists("/.b.tailraid-upload") {
		t.Fatal("a cut-off upload created something")
	}
}

// Invariant: I-81
func TestWebUI_Put_ShortBody_400_NoTemp(t *testing.T) {
	u := newUI(t)
	code, body := u.putCut("/s", 1000, testharness.Pattern(500, 5))
	if code != 400 || !strings.Contains(fmt.Sprint(body["error"]), "500 of 1000") {
		t.Fatalf("short body: %d %v", code, body)
	}
	if u.exists("/s") || u.exists("/.s.tailraid-upload") {
		t.Fatal("a short upload left something behind")
	}
}

// Invariant: I-82
func TestWebUI_Put_NoContentLength_411(t *testing.T) {
	u := newUI(t)
	conn, err := net.Dial("tcp", u.hs.Listener.Addr().String())
	must(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "PUT /files/c HTTP/1.1\r\nHost: tailraid\r\n%s: 1\r\nTransfer-Encoding: chunked\r\n\r\n7\r\nchunked\r\n0\r\n\r\n", webui.HeaderUI)
	must(t, err)
	if code, _, _ := u.reply(conn); code != http.StatusLengthRequired {
		t.Fatalf("chunked PUT: %d", code)
	}
	if u.exists("/c") || u.exists("/.c.tailraid-upload") {
		t.Fatal("a length-less upload left something behind")
	}
}

// Parts land in order into the temp, a part at the wrong offset is refused with the
// offset to continue from, the last byte publishes the file, and the final part may be
// any size.
// Invariant: I-81
func TestWebUI_Put_Parts_Table(t *testing.T) {
	cs := int(testharness.ChunkSize)
	for _, lastSize := range []int{1, cs - 1, cs, cs + 1} {
		t.Run(fmt.Sprint(lastSize), func(t *testing.T) {
			u := newUI(t)
			data := testharness.Pattern(2*cs+lastSize, byte(lastSize))
			total := len(data)
			if code, body := u.putPart("/p", data[:cs], 0, total); code != 202 || body["offset"] != float64(cs) {
				t.Fatalf("part 0: %d %v", code, body)
			}
			if st := u.statOf("/.p.tailraid-upload"); st.Size != uint64(cs) || !st.Upload {
				t.Fatalf("temp after part 0: %+v", st)
			}
			if code, body := u.putPart("/p", data[:cs], 0, total); code != 409 || body["offset"] != float64(cs) {
				t.Fatalf("part at the wrong offset: %d %v", code, body)
			}
			if code, body := u.putPart("/p", data[cs:2*cs], cs, total); code != 202 || body["offset"] != float64(2*cs) {
				t.Fatalf("part 1: %d %v", code, body)
			}
			if u.exists("/p") {
				t.Fatal("published before the last part")
			}
			if code, _ := u.putPart("/p", data[2*cs:], 2*cs, total); code != 201 {
				t.Fatalf("last part (%d bytes): %d", lastSize, code)
			}
			if got := u.readAt("/p", 0, int64(total)+1); !bytes.Equal(got, data) {
				t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
			}
			if u.exists("/.p.tailraid-upload") {
				t.Fatal("temp left after publishing")
			}
		})
	}
	// a whole file of zero bytes, and a range that runs past its total
	u := newUI(t)
	if code, _ := u.put("/z", nil); code != 201 {
		t.Fatalf("empty file: %d", code)
	}
	if st := u.statOf("/z"); st.Size != 0 {
		t.Fatalf("empty file: %+v", st)
	}
	if code, _ := u.putPart("/q", testharness.Pattern(10, 1), 5, 10); code != 400 {
		t.Fatalf("range past the total: %d", code)
	}
}

// A part cut off mid-way is rolled back to its start: the next attempt at that offset is
// accepted and the file comes out exact.
// Invariant: I-81
func TestWebUI_Put_Resume_AfterFailedPart(t *testing.T) {
	u := newUI(t)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(3*cs, 6)
	if code, _ := u.putPart("/r", data[:cs], 0, len(data)); code != 202 {
		t.Fatalf("part 0: %d", code)
	}
	code, _ := u.putCut("/r", cs, data[cs:cs+100], "Content-Range", fmt.Sprintf("bytes %d-%d/%d", cs, 2*cs-1, len(data)))
	if code != 400 {
		t.Fatalf("cut-off part: %d", code)
	}
	if st := u.statOf("/.r.tailraid-upload"); st.Size != uint64(cs) {
		t.Fatalf("temp not rolled back to the part's start: %+v", st)
	}
	if code, body := u.putPart("/r", data[cs:2*cs], cs, len(data)); code != 202 {
		t.Fatalf("resumed part: %d %v", code, body)
	}
	if code, _ := u.putPart("/r", data[2*cs:], 2*cs, len(data)); code != 201 {
		t.Fatalf("last part: %d", code)
	}
	if got := u.readAt("/r", 0, int64(len(data))+1); !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
}

// An announced size the volume cannot hold at the policy is refused before any body
// byte is sent.
// Invariant: I-82
func TestWebUI_Put_FreeSpace_507(t *testing.T) {
	u := newUI(t, func(o *webui.Options) {
		o.Status = func() any {
			return admin.Report{Volume: admin.VolumeReport{K: 2, M: 1, Free: 3000}}
		}
	})
	conn := u.rawPut("/big", 2001, nil) // 2001 × 3/2 > 3000; no body byte is ever sent
	defer conn.Close()
	if code, _, _ := u.reply(conn); code != http.StatusInsufficientStorage {
		t.Fatalf("too big: %d", code)
	}
	if u.exists("/.big.tailraid-upload") {
		t.Fatal("a refused upload created its temp")
	}
	if code, _ := u.put("/fits", testharness.Pattern(2000, 1)); code != 201 { // 2000 × 3/2 = 3000 fits
		t.Fatalf("fits: %d", code)
	}
}

// A fifth concurrent upload is refused with 503 and Retry-After while four hold slots.
// Invariant: I-82
func TestWebUI_Put_Concurrency_503(t *testing.T) {
	u := newUI(t, func(o *webui.Options) { o.BodyTimeout = time.Minute })
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := range 4 {
		conn := u.rawPut(fmt.Sprintf("/slot%d", i), 10, nil)
		conns = append(conns, conn)
		name := fmt.Sprintf("/.slot%d.tailraid-upload", i) // the temp exists once the handler holds a slot
		testharness.Eventually(t, func() bool { return u.exists(name) }, "upload "+name+" started")
	}
	resp := u.do(http.MethodPut, "/files/fifth", bytes.NewReader([]byte("x")), webui.HeaderUI, "1")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("fifth upload: %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	for _, conn := range conns {
		_, err := conn.Write(bytes.Repeat([]byte("y"), 10))
		must(t, err)
		if code, _, _ := u.reply(conn); code != 201 {
			t.Fatalf("slot upload: %d", code)
		}
	}
	if code, _ := u.put("/fifth", []byte("x")); code != 201 {
		t.Fatalf("after the slots freed: %d", code)
	}
}

// A body that sends nothing for BodyTimeout is dropped together with its temp.
// Invariant: I-82
func TestWebUI_Put_BodyStall_DropsTemp(t *testing.T) {
	u := newUI(t, func(o *webui.Options) { o.BodyTimeout = 100 * time.Millisecond })
	conn := u.rawPut("/stall", 1000, bytes.Repeat([]byte("s"), 10))
	defer conn.Close()
	if code, _, _ := u.reply(conn); code != http.StatusRequestTimeout {
		t.Fatalf("stalled body: %d", code)
	}
	if u.exists("/stall") || u.exists("/.stall.tailraid-upload") {
		t.Fatal("the stalled upload left something behind")
	}
}

// Temps older than the TTL are swept, younger ones kept, at the boundary.
// Invariant: I-81
func TestWebUI_SweepUploads_AgeTable(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		gone bool
	}{{webui.UploadTTL - time.Second, false}, {webui.UploadTTL, false}, {webui.UploadTTL + time.Second, true}} { // exactly the TTL old is still kept (docs/06: one grace rule)
		t.Run(c.age.String(), func(t *testing.T) {
			u := newUI(t)
			must(t, u.fs.MkdirAll("/d", 0o755))
			u.write("/d/.movie.tailraid-upload", 0, []byte("partial"))
			u.write("/d/movie.mkv", 0, []byte("real")) // a real file of the same age stays
			u.flush()
			now := u.f.Clk.Now().Add(c.age)
			n, err := webui.SweepUploads(context.Background(), func(meta.UserIdx) webui.Remover { return u.fs }, u.f.FSM, now, webui.UploadTTL)
			must(t, err)
			if gone := !u.exists("/d/.movie.tailraid-upload"); gone != c.gone || (c.gone && n != 1) || (!c.gone && n != 0) {
				t.Fatalf("age %v: removed %d, temp gone %v, want gone %v", c.age, n, gone, c.gone)
			}
			if !u.exists("/d/movie.mkv") {
				t.Fatal("the real file was swept")
			}
		})
	}
}

// Listings and stat mark an upload's temp so the browser labels it and the player skips it.
func TestWebUI_Ls_MarksUploads(t *testing.T) {
	u := newUI(t)
	u.write("/.song.tailraid-upload", 0, []byte("half"))
	u.write("/song.flac", 0, []byte("whole"))
	code, _, body := u.get("/api/ls?path=/")
	if code != 200 {
		t.Fatalf("ls: %d", code)
	}
	var out struct{ Entries []webui.Entry }
	must(t, json.Unmarshal(body, &out))
	marks := map[string]bool{}
	for _, e := range out.Entries {
		marks[e.Name] = e.Upload
	}
	if !marks[".song.tailraid-upload"] || marks["song.flac"] {
		t.Fatalf("upload marks: %v", marks)
	}
	if st := u.statOf("/.song.tailraid-upload"); !st.Upload {
		t.Fatalf("stat: %+v", st)
	}
	if !webui.IsUploadTemp(".x.tailraid-upload") || webui.IsUploadTemp(".tailraid-upload") || webui.IsUploadTemp("x.tailraid-upload") {
		t.Fatal("IsUploadTemp boundaries")
	}
	_ = io.EOF
}

// The browser lists what the user put in the volume, not what macOS wrote beside it:
// "._<name>" (a file's extended attributes over NFS) and ".DS_Store" are left out of a
// listing, every other dot file (an upload's temp included) stays, and a left-out file
// is still in the volume and readable by its path.
// Invariant: I-140
func TestWebUI_Ls_HidesOSMetadata(t *testing.T) {
	u := newUI(t)
	for _, name := range []string{"/photo.jpg", "/._photo.jpg", "/.DS_Store", "/.config", "/._", "/.song.tailraid-upload"} {
		u.write(name, 0, []byte("x"))
	}
	code, _, body := u.get("/api/ls?path=/")
	if code != 200 {
		t.Fatalf("ls: %d", code)
	}
	var out struct{ Entries []webui.Entry }
	must(t, json.Unmarshal(body, &out))
	got := map[string]bool{}
	for _, e := range out.Entries {
		got[e.Name] = true
	}
	for name, want := range map[string]bool{"photo.jpg": true, "._photo.jpg": false, ".DS_Store": false, ".config": true, "._": true, ".song.tailraid-upload": true} {
		if got[name] != want {
			t.Errorf("%s listed %v, want %v (listing %v)", name, got[name], want, got)
		}
	}
	if code, _, body := u.get("/files/._photo.jpg"); code != 200 || string(body) != "x" {
		t.Errorf("a left-out file by its path: %d %q", code, body)
	}
}

// The node's own tree, /.tailraid (thumbnails, the stats segments of docs/17), is absent
// from the root listing while its files stay reachable by path, like the OS metadata.
// Invariant: I-195
func TestWebUI_Ls_HidesTailraidTree(t *testing.T) {
	u := newUI(t)
	u.write("/.tailraid/stats/10s/pike/2026-10-11T14-pike.jsonl.gz", 0, []byte("x"))
	u.write("/photo.jpg", 0, []byte("y"))
	code, _, body := u.get("/api/ls?path=/")
	if code != 200 {
		t.Fatalf("ls: %d", code)
	}
	var out struct{ Entries []webui.Entry }
	must(t, json.Unmarshal(body, &out))
	for _, e := range out.Entries {
		if e.Name == ".tailraid" {
			t.Fatalf("the reserved tree is listed: %+v", out.Entries)
		}
	}
	if len(out.Entries) != 1 || out.Entries[0].Name != "photo.jpg" {
		t.Fatalf("listing %+v", out.Entries)
	}
	if code, _, body := u.get("/api/ls?path=/.tailraid/stats/10s/pike"); code != 200 || !strings.Contains(string(body), "2026-10-11T14-pike.jsonl.gz") {
		t.Errorf("the tree by its path: %d %s", code, body)
	}
	if code, _, body := u.get("/files/.tailraid/stats/10s/pike/2026-10-11T14-pike.jsonl.gz"); code != 200 || string(body) != "x" {
		t.Errorf("a segment by its path: %d %q", code, body)
	}
}

// Once users exist, an upload's temp carries its uploader and the sweep removes a stale
// one as them: the node's own view may write only under /.tailraid, so a sweep as the
// node was refused and stopped at its first temp. A temp the sweep cannot remove (here
// one from before users, named for nobody) is left and the walk goes on.
// Invariant: I-81
func TestWebUI_SweepUploads_AsTheUploader(t *testing.T) {
	u, kim, lee := usersUI(t)
	b := u.signIn(lee)
	resp := u.doWith(b, http.MethodPut, "/files/w/movie.mkv", strings.NewReader("half"), webui.HeaderUI, "1", "Content-Range", "bytes 0-3/10")
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("part 0 by lee: %d", resp.StatusCode)
	}
	var temp string
	names, err := u.fs.ReadDir("/w")
	must(t, err)
	for _, n := range names {
		if webui.IsUploadTemp(n.Name()) {
			temp = n.Name()
		}
	}
	if webui.UploadTempUser(temp) != lee {
		t.Fatalf("temp %q names user %d, want lee (%d)", temp, webui.UploadTempUser(temp), lee)
	}
	fh, err := u.fs.As(kim).OpenFile("/other/.old.tailraid-upload", os.O_RDWR|os.O_CREATE, 0o644)
	must(t, err)
	_, err = fh.Write([]byte("from before users"))
	must(t, err)
	must(t, fh.Close())
	u.flush()
	as := func(user meta.UserIdx) webui.Remover {
		if user == 0 {
			return u.fs
		}
		return u.fs.As(user)
	}
	n, err := webui.SweepUploads(context.Background(), as, u.f.FSM, u.f.Clk.Now().Add(webui.UploadTTL+time.Second), webui.UploadTTL)
	if n != 1 || u.exists("/w/"+temp) {
		t.Fatalf("swept %d (%v); lee's temp gone: %v", n, err, !u.exists("/w/"+temp))
	}
}
