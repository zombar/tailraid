package webui_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// post sends a JSON body with the UI header and decodes the reply into out (nil to
// discard); the status is returned.
func (u *ui) post(route, body string, out any) int {
	u.t.Helper()
	resp := u.do(http.MethodPost, route, strings.NewReader(body), webui.HeaderUI, "1", "Content-Type", "application/json")
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	if out != nil && b.Len() > 0 {
		if err := json.Unmarshal(b.Bytes(), out); err != nil {
			u.t.Fatalf("%s: %d %s: %v", route, resp.StatusCode, b.String(), err)
		}
	}
	return resp.StatusCode
}

type listing struct {
	Path    string        `json:"path"`
	Dir     webui.DirInfo `json:"dir"`
	Entries []webui.Entry `json:"entries"`
}

func (u *ui) lsOf(query string) listing {
	u.t.Helper()
	code, _, body := u.get("/api/ls?" + query)
	if code != 200 {
		u.t.Fatalf("ls %s: %d %s", query, code, body)
	}
	var l listing
	must(u.t, json.Unmarshal(body, &l))
	return l
}

func (l listing) entry(name string) *webui.Entry {
	for i := range l.Entries {
		if l.Entries[i].Name == name {
			return &l.Entries[i]
		}
	}
	return nil
}

// deleted is the listing's deleted root called name, or nil.
func (l listing) deleted(name string) *webui.Entry {
	for i := range l.Entries {
		if l.Entries[i].Name == name && l.Entries[i].DeletedAt != "" {
			return &l.Entries[i]
		}
	}
	return nil
}

type batch struct {
	Moved   []string        `json:"moved"`
	Deleted []string        `json:"deleted"`
	Failed  []webui.Failure `json:"failed"`
}

func (b batch) failure(path string) *webui.Failure {
	for i := range b.Failed {
		if b.Failed[i].Path == path {
			return &b.Failed[i]
		}
	}
	return nil
}

func (u *ui) del(paths ...string) batch {
	u.t.Helper()
	body, _ := json.Marshal(map[string]any{"paths": paths})
	var out batch
	if code := u.post("/api/delete", string(body), &out); code != 200 {
		u.t.Fatalf("delete %v: %d", paths, code)
	}
	return out
}

func (u *ui) inoOf(path string) meta.Ino {
	u.t.Helper()
	in, err := u.f.FSM.Resolve(meta.SplitPath(path))
	must(u.t, err)
	return in.Ino
}

func (u *ui) setFlags(path, body string) (int, webui.Entry) {
	u.t.Helper()
	var e webui.Entry
	code := u.post("/api/flags", fmt.Sprintf(`{"path":%q,%s}`, path, body), &e)
	return code, e
}

// A delete through the web lands in the trash: the entry leaves the listing's live
// part and reappears after it with deleted_at and purge_at, the share no longer resolves
// it, a folder goes as one unit, and the dashboard's /api/trash lists it with its path.
//
// Invariant: I-158
func TestWebUI_Delete_LandsInTrash_ListedInPlace(t *testing.T) {
	u := newUI(t, func(o *webui.Options) { o.Retention = 30 * 24 * time.Hour })
	u.write("/d/f", 0, []byte("hello"))
	u.write("/d/sub/g", 0, []byte("deep"))
	u.write("/d/sub/h", 0, []byte("deeper"))
	u.flush()
	fIno, subIno := u.inoOf("/d/f"), u.inoOf("/d/sub")
	res := u.del("/d/f")
	u.f.Clk.Advance(time.Second)
	res2 := u.del("/d/sub")
	if len(res.Deleted) != 1 || len(res.Failed) != 0 || len(res2.Deleted) != 1 {
		t.Fatalf("delete: %+v %+v", res, res2)
	}
	if u.exists("/d/f") || u.exists("/d/sub/g") {
		t.Fatal("the share still resolves a deleted entry")
	}
	l := u.lsOf("path=/d")
	if len(l.Entries) != 2 || l.Entries[0].DeletedAt == "" || l.Entries[1].DeletedAt == "" {
		t.Fatalf("listing after the delete: %+v", l.Entries)
	}
	f, sub := l.deleted("f"), l.deleted("sub")
	if f == nil || sub == nil || meta.Ino(f.Ino) != fIno || meta.Ino(sub.Ino) != subIno || sub.Type != "dir" {
		t.Fatalf("deleted roots: %+v %+v", f, sub)
	}
	deletedAt, err := time.Parse(time.RFC3339, f.DeletedAt)
	must(t, err)
	purgeAt, err := time.Parse(time.RFC3339, f.PurgeAt)
	must(t, err)
	if purgeAt.Sub(deletedAt) != 30*24*time.Hour {
		t.Fatalf("purge_at %s for deleted_at %s", f.PurgeAt, f.DeletedAt)
	}
	// newest first: sub was deleted a second after f, so it comes first
	if l.Entries[0].Name != "sub" {
		t.Fatalf("deleted roots order: %s then %s", l.Entries[0].Name, l.Entries[1].Name)
	}
	var tr struct {
		Entries []struct {
			Ino        uint64 `json:"ino"`
			Path       string `json:"path"`
			Restorable bool   `json:"restorable"`
		} `json:"entries"`
	}
	code, _, body := u.get("/api/trash")
	must(t, json.Unmarshal(body, &tr))
	if code != 200 || len(tr.Entries) != 2 || tr.Entries[0].Path != "/d/sub" || !tr.Entries[0].Restorable || tr.Entries[1].Path != "/d/f" {
		t.Fatalf("trash listing: %d %+v", code, tr.Entries)
	}
	// a live file may take the name at once; the deleted one keeps its own
	u.write("/d/f", 0, []byte("new"))
	l = u.lsOf("path=/d")
	if live := l.entry("f"); live == nil || live.DeletedAt != "" || l.deleted("f") == nil {
		t.Fatalf("after re-creating f: %+v", l.Entries)
	}
	// a delete the share would refuse: a protected folder; and the reserved folder
	if code, _ := u.setFlags("/d", `"protected":true`); code != 200 {
		t.Fatalf("protect: %d", code)
	}
	res = u.del("/d/f", "/.tailraid/x", "/")
	if len(res.Deleted) != 0 || len(res.Failed) != 3 || res.failure("/d/f").Status != 403 || res.failure("/.tailraid/x").Status != 400 || res.failure("/").Status != 400 {
		t.Fatalf("refused deletes: %+v", res)
	}
}

// in=<ino> addresses a deleted root: ls and stat read inside it, files serves the same
// bytes the vfs served before the delete, with the ETag of the same (ino, gen,
// version); the dir block carries the root's deleted_at; a live path inside it is 404.
//
// Invariant: I-158
func TestWebUI_TrashAddressing_LsStatGet(t *testing.T) {
	u := newUI(t)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(2*cs+7, 4)
	u.write("/a/v.bin", 0, data)
	u.write("/a/n/t.txt", 0, []byte("note"))
	u.flush()
	before := u.readAt("/a/v.bin", 0, int64(len(data)))
	code, hdr, _ := u.get("/files/a/v.bin")
	etag := hdr.Get("ETag")
	if code != 200 || etag == "" {
		t.Fatalf("live get: %d %q", code, etag)
	}
	root := u.inoOf("/a")
	if res := u.del("/a"); len(res.Deleted) != 1 {
		t.Fatalf("delete: %+v", res)
	}
	in := fmt.Sprint(uint64(root))
	l := u.lsOf("path=/&in=" + in)
	if l.Dir.DeletedAt == "" || len(l.Entries) != 2 || l.entry("v.bin") == nil || l.entry("n") == nil || l.entry("v.bin").Size != uint64(len(data)) {
		t.Fatalf("ls inside the root: %+v", l)
	}
	if l2 := u.lsOf("path=/n&in=" + in); len(l2.Entries) != 1 || l2.Entries[0].Name != "t.txt" || l2.Path != "/n" {
		t.Fatalf("ls deeper: %+v", l2)
	}
	code, _, body := u.get("/api/stat?path=/v.bin&in=" + in)
	var st webui.Entry
	must(t, json.Unmarshal(body, &st))
	if code != 200 || st.Name != "v.bin" || st.DeletedAt == "" || st.Path != "/v.bin" {
		t.Fatalf("stat inside: %d %+v", code, st)
	}
	code, hdr, got := u.get("/files/v.bin?in=" + in)
	if code != 200 || !bytes.Equal(got, before) || hdr.Get("ETag") != etag || hdr.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("get inside: %d, etag %q (live %q), differs at %d", code, hdr.Get("ETag"), etag, testharness.FirstDiff(got, before))
	}
	code, hdr, got = u.get("/files/v.bin?in="+in, "Range", "bytes=10-19")
	if code != 206 || !bytes.Equal(got, before[10:20]) || hdr.Get("Content-Range") != fmt.Sprintf("bytes 10-19/%d", len(data)) {
		t.Fatalf("range inside: %d %q", code, hdr.Get("Content-Range"))
	}
	for _, q := range []string{"/files/a/v.bin", "/api/ls?path=/a", "/api/stat?path=/a/v.bin", "/api/ls?path=/&in=999", "/api/ls?path=/&in=x", "/api/ls?path=/&in=1"} {
		if code, _, _ := u.get(q); code != 404 && code != 400 {
			t.Fatalf("%s: %d, want 404 or 400", q, code)
		}
	}
}

// Restore puts a deleted root back with its (ino, gen): in its place; a taken name is
// 409 {taken} and the page's second try with a name lands; a root whose folder is
// itself deleted is 409 {in_trash} and lands with a destination; not a root is 404.
//
// Invariant: I-158
func TestWebUI_Restore_TakenName_409_ThenAs(t *testing.T) {
	u := newUI(t)
	u.write("/d/f", 0, []byte("one"))
	u.flush()
	in, err := u.f.FSM.Resolve(meta.SplitPath("/d/f"))
	must(t, err)
	u.del("/d/f")
	var out map[string]any
	if code := u.post("/api/restore", fmt.Sprintf(`{"ino":%d}`, in.Ino), &out); code != 200 || out["path"] != "/d/f" {
		t.Fatalf("restore in place: %d %v", code, out)
	}
	back, err := u.f.FSM.Resolve(meta.SplitPath("/d/f"))
	must(t, err)
	if back.Ino != in.Ino || back.Gen != in.Gen || string(u.readAt("/d/f", 0, 3)) != "one" {
		t.Fatalf("restored identity %+v, want %+v", back, in)
	}
	// the name is taken
	u.del("/d/f")
	u.write("/d/f", 0, []byte("two"))
	if code := u.post("/api/restore", fmt.Sprintf(`{"ino":%d}`, in.Ino), &out); code != 409 || out["taken"] != true {
		t.Fatalf("restore over a taken name: %d %v", code, out)
	}
	if code := u.post("/api/restore", fmt.Sprintf(`{"ino":%d,"name":"f (restored)"}`, in.Ino), &out); code != 200 || out["path"] != "/d/f (restored)" {
		t.Fatalf("restore as: %d %v", code, out)
	}
	if string(u.readAt("/d/f (restored)", 0, 3)) != "one" || string(u.readAt("/d/f", 0, 3)) != "two" {
		t.Fatal("restored bytes")
	}
	// the folder is itself deleted: c's parent b sits inside the deleted a
	u.write("/a/b/c", 0, []byte("c"))
	u.flush()
	cIno := u.inoOf("/a/b/c")
	u.del("/a/b/c")
	u.del("/a")
	if code := u.post("/api/restore", fmt.Sprintf(`{"ino":%d}`, cIno), &out); code != 409 || out["in_trash"] != true {
		t.Fatalf("restore under a deleted folder: %d %v", code, out)
	}
	if code := u.post("/api/restore", fmt.Sprintf(`{"ino":%d,"to":"/d"}`, cIno), &out); code != 200 || out["path"] != "/d/c" {
		t.Fatalf("restore elsewhere: %d %v", code, out)
	}
	for _, body := range []string{`{"ino":424242}`, fmt.Sprintf(`{"ino":%d}`, u.inoOf("/d/c"))} { // unknown, and live
		if code := u.post("/api/restore", body, nil); code != 404 {
			t.Fatalf("restore %s: %d", body, code)
		}
	}
	if code := u.post("/api/restore", fmt.Sprintf(`{"ino":%d,"to":"/.tailraid"}`, cIno), nil); code != 400 {
		t.Fatalf("restore into the reserved folder: %d", code)
	}
}

// Purge drops the entry's references: its sole chunk is tombstoned and the trash empties;
// {all} empties a trash of many entries; a purge of nothing is 404 and no selection 400.
//
// Invariant: I-158
func TestWebUI_Purge_FreesRefs(t *testing.T) {
	u := newUI(t)
	cs := int(testharness.ChunkSize)
	u.write("/f", 0, testharness.Pattern(cs, 1))
	u.flush()
	in, err := u.f.FSM.Resolve(meta.SplitPath("/f"))
	must(t, err)
	hash := in.Chunks[0]
	u.del("/f")
	if ch, ok := u.f.FSM.GetChunk(hash); !ok || ch.Refs != 1 {
		t.Fatalf("after the delete: %+v %v", ch, ok)
	}
	if code := u.post("/api/purge", fmt.Sprintf(`{"ino":%d}`, in.Ino), nil); code != 204 {
		t.Fatalf("purge: %d", code)
	}
	if ch, ok := u.f.FSM.GetChunk(hash); ok && (ch.Refs != 0 || ch.TombstonedAt == 0) {
		t.Fatalf("after the purge: %+v", ch)
	}
	if st := u.f.FSM.Stats(); st.TrashInodes != 0 {
		t.Fatalf("trash after the purge: %+v", st)
	}
	if code := u.post("/api/purge", fmt.Sprintf(`{"ino":%d}`, in.Ino), nil); code != 404 {
		t.Fatalf("purge again: %d", code)
	}
	if code := u.post("/api/purge", `{}`, nil); code != 400 {
		t.Fatalf("purge nothing: %d", code)
	}
	for i := range 5 {
		u.write(fmt.Sprintf("/g%d", i), 0, []byte("x"))
	}
	for i := range 5 {
		u.del(fmt.Sprintf("/g%d", i))
	}
	if code := u.post("/api/purge", `{"all":true}`, nil); code != 204 || u.f.FSM.Stats().TrashInodes != 0 {
		t.Fatalf("purge all: %d, %+v", code, u.f.FSM.Stats())
	}
}

// A PUT over an existing name keeps the previous version in the trash under its old
// name, readable through in=; the upload's temp never enters the trash.
//
// Invariant: I-158
func TestWebUI_Put_OverwriteKeepsPreviousInTrash(t *testing.T) {
	u := newUI(t)
	if code, _ := u.put("/x", []byte("version one")); code != 201 {
		t.Fatalf("first put: %d", code)
	}
	u.flush()
	first := u.inoOf("/x")
	if code, _ := u.put("/x", []byte("version two")); code != 204 {
		t.Fatalf("overwrite: %d", code)
	}
	l := u.lsOf("path=/")
	live, old := l.entry("x"), l.deleted("x")
	if live == nil || live.DeletedAt != "" || old == nil || meta.Ino(old.Ino) != first || len(l.Entries) != 2 {
		t.Fatalf("after the overwrite: %+v", l.Entries)
	}
	if code, _, body := u.get(fmt.Sprintf("/files/?in=%d", first)); code != 200 || string(body) != "version one" {
		t.Fatalf("previous version: %d %q", code, body)
	}
	if code, _, body := u.get("/files/x"); code != 200 || string(body) != "version two" {
		t.Fatalf("live version: %d %q", code, body)
	}
	if roots := u.f.FSM.Trash(); len(roots) != 1 || webui.IsUploadTemp(roots[0].Name) {
		t.Fatalf("trash: %+v", roots)
	}
}

// Rename: the new name's bounds {0, 1, max−1, max, max+1}, the names no entry may
// have, a taken name, the root, the same name, a file inside a protected folder (its
// folder is unchanged: allowed) and the protected folder itself (refused).
//
// Invariant: I-159
func TestWebUI_Rename_Table(t *testing.T) {
	u := newUI(t)
	u.write("/d/a", 0, []byte("a"))
	u.write("/d/taken", 0, []byte("t"))
	u.write("/p/in", 0, []byte("i"))
	u.flush()
	if code, _ := u.setFlags("/p", `"protected":true`); code != 200 {
		t.Fatalf("protect: %d", code)
	}
	long := strings.Repeat("n", meta.MaxNameLen)
	for _, tc := range []struct {
		path, name string
		want       int
	}{
		{"/d/a", "", 400},
		{"/d/a", "b", 200},
		{"/d/b", long[:len(long)-1], 200},
		{"/d/" + long[:len(long)-1], long, 200},
		{"/d/" + long, long + "n", 400},
		{"/d/" + long, ".", 400},
		{"/d/" + long, "..", 400},
		{"/d/" + long, "x/y", 400},
		{"/d/" + long, "taken", 409},
		{"/d/" + long, long, 200}, // the same name
		{"/", "root", 400},
		{"/nope", "x", 404},
		{"/p/in", "inside", 200},
		{"/p", "q", 403},
		{"/.tailraid", "q", 400},
	} {
		var out map[string]any
		code := u.post("/api/rename", fmt.Sprintf(`{"path":%q,"name":%q}`, tc.path, tc.name), &out)
		if code != tc.want {
			t.Fatalf("rename %s to %q: %d %v, want %d", tc.path, tc.name, code, out, tc.want)
		}
		if code == 200 && out["path"] != strings.TrimSuffix(tc.path, "/"+tc.path[strings.LastIndex(tc.path, "/")+1:])+"/"+tc.name {
			t.Fatalf("rename %s to %q answered %v", tc.path, tc.name, out)
		}
	}
	if !u.exists("/p/inside") || !u.exists("/p") || u.exists("/q") {
		t.Fatal("the protected folder moved, or the file inside did not")
	}
}

// Move tries every path on its own and reports each: a moved one is under its new
// name, a taken name is 409, a path under a protected folder 403, a missing one 404,
// the root 400, a folder into itself 400; none of the failures touches the others, and
// `to` must be a live folder.
//
// Invariant: I-159
func TestWebUI_Move_PerItemResults(t *testing.T) {
	u := newUI(t)
	for _, p := range []string{"/a/f1", "/a/f2", "/b/f2", "/p/f3", "/a/sub/x"} {
		u.write(p, 0, []byte(p))
	}
	u.flush()
	if code, _ := u.setFlags("/p", `"protected":true`); code != 200 {
		t.Fatalf("protect: %d", code)
	}
	var out batch
	code := u.post("/api/move", `{"paths":["/a/f1","/a/f2","/p/f3","/nope","/","/a/sub"],"to":"/b"}`, &out)
	if code != 200 || len(out.Moved) != 2 || out.Moved[0] != "/b/f1" || out.Moved[1] != "/b/sub" || len(out.Failed) != 4 {
		t.Fatalf("move: %d %+v", code, out)
	}
	for path, want := range map[string]int{"/a/f2": 409, "/p/f3": 403, "/nope": 404, "/": 400} {
		if f := out.failure(path); f == nil || f.Status != want {
			t.Fatalf("%s: %+v, want %d", path, f, want)
		}
	}
	if !u.exists("/b/f1") || !u.exists("/a/f2") || !u.exists("/p/f3") || !u.exists("/b/sub/x") || string(u.readAt("/b/f2", 0, 5)) != "/b/f2" {
		t.Fatal("the failures touched other entries, or a move did not land")
	}
	// a folder into itself, and into a protected folder (allowed)
	code = u.post("/api/move", `{"paths":["/b/sub"],"to":"/b/sub"}`, &out)
	if code != 200 || len(out.Failed) != 1 || out.Failed[0].Status != 400 {
		t.Fatalf("move into itself: %d %+v", code, out)
	}
	code = u.post("/api/move", `{"paths":["/b/f1"],"to":"/p"}`, &out)
	if code != 200 || len(out.Moved) != 1 || !u.exists("/p/f1") {
		t.Fatalf("move into a protected folder: %d %+v", code, out)
	}
	for body, want := range map[string]int{`{"paths":["/b/f2"],"to":"/nope"}`: 404, `{"paths":["/b/f2"],"to":"/b/f2"}`: 400, `{"paths":["/b/f2"],"to":"/.tailraid"}`: 400} {
		if code := u.post("/api/move", body, nil); code != want {
			t.Fatalf("%s: %d, want %d", body, code, want)
		}
	}
}

// A batch of {0, 1, max−1, max, max+1} paths: none is 400, the bound and under are
// handled, one over is 413 before any entry is touched.
//
// Invariant: I-159
func TestWebUI_Batch_SizeBoundaries(t *testing.T) {
	u := newUI(t)
	u.write("/keep", 0, []byte("k"))
	u.flush()
	for _, n := range []int{0, 1, webui.MaxBatch - 1, webui.MaxBatch, webui.MaxBatch + 1} {
		paths := make([]string, n)
		for i := range paths {
			paths[i] = fmt.Sprintf("/nope%d", i)
		}
		if n > 0 {
			paths[0] = "/keep"
		}
		body, _ := json.Marshal(map[string]any{"paths": paths, "to": "/"})
		want := 200
		switch {
		case n == 0:
			want = 400
		case n > webui.MaxBatch:
			want = 413
		}
		for _, route := range []string{"/api/delete", "/api/move"} {
			var out batch
			if code := u.post(route, string(body), &out); code != want {
				t.Fatalf("%s with %d paths: %d, want %d", route, n, code, want)
			}
			if want == 200 && len(out.Deleted)+len(out.Moved)+len(out.Failed) != n {
				t.Fatalf("%s with %d paths reported %d items", route, n, len(out.Deleted)+len(out.Moved)+len(out.Failed))
			}
		}
		if n > webui.MaxBatch && !u.exists("/keep") {
			t.Fatal("an oversize batch touched an entry")
		}
		u.write("/keep", 0, []byte("k")) // deleted by the in-bound batches
	}
}

// Every mutating route reads a bounded body (docs/08 I-116): one byte over is a 400
// that says "too large", the bound itself is read.
//
// Invariant: I-116, I-159
func TestWebUI_BodyCaps_AllMutatingRoutes(t *testing.T) {
	u := newUI(t)
	u.write("/f", 0, []byte("f"))
	u.flush()
	for _, tc := range []struct {
		route string
		limit int
	}{{"/api/rename", 4096}, {"/api/restore", 4096}, {"/api/purge", 4096}, {"/api/flags", 4096}, {"/api/pin", 4096}, {"/api/move", 256 << 10}, {"/api/delete", 256 << 10}} {
		for _, n := range []int{tc.limit - 1, tc.limit, tc.limit + 1} {
			head, tail := `{"path":"/f","paths":["/f"],"to":"/","name":"g","ino":1,"all":false,"pad":"`, `"}`
			body := head + strings.Repeat("a", n-len(head)-len(tail)) + tail
			resp := u.do(http.MethodPost, tc.route, strings.NewReader(body), webui.HeaderUI, "1")
			var b bytes.Buffer
			_, _ = b.ReadFrom(resp.Body)
			resp.Body.Close()
			over := n > tc.limit
			if over && (resp.StatusCode != 400 || !strings.Contains(b.String(), "too large")) {
				t.Fatalf("%s %d bytes: %d %s, want 400 too large", tc.route, n, resp.StatusCode, b.String())
			}
			if !over && strings.Contains(b.String(), "too large") {
				t.Fatalf("%s %d bytes refused as too large", tc.route, n)
			}
		}
		u.write("/f", 0, []byte("f"))
	}
	// the thumbnail PUT's bound is its own table (TestWebUI_Thumb_Put_Boundaries)
}

// Flags: directories only, never the root or /.tailraid; ls and stat carry the
// effective value (an ancestor's) next to each entry's own.
//
// Invariant: I-163
func TestWebUI_Flags_DirsOnly_Effective(t *testing.T) {
	u := newUI(t)
	u.write("/top/mid/leaf.txt", 0, []byte("x"))
	u.flush()
	for path, body := range map[string]string{"/top/mid/leaf.txt": `"nsfw":true`, "/": `"protected":true`, "/.tailraid": `"nsfw":true`} {
		if code, _ := u.setFlags(path, body); code != 400 {
			t.Fatalf("flags on %s: %d", path, code)
		}
	}
	if code, _ := u.setFlags("/nope", `"nsfw":true`); code != 404 {
		t.Fatal("flags on nothing")
	}
	code, e := u.setFlags("/top", `"nsfw":true,"protected":true`)
	if code != 200 || !e.NSFW || !e.Protected || e.Path != "/top" {
		t.Fatalf("set both: %d %+v", code, e)
	}
	code, e = u.setFlags("/top", `"protected":false`)
	if code != 200 || !e.NSFW || e.Protected {
		t.Fatalf("clear one: %d %+v", code, e)
	}
	l := u.lsOf("path=/top/mid")
	if !l.Dir.NSFW || l.Dir.Protected || l.entry("leaf.txt").NSFW {
		t.Fatalf("effective flags of /top/mid: %+v, leaf own %+v", l.Dir, l.entry("leaf.txt"))
	}
	if top := u.lsOf("path=/").entry("top"); !top.NSFW || top.Protected {
		t.Fatalf("own flags of /top in the root listing: %+v", top)
	}
	if st := u.statOf("/top/mid/leaf.txt"); !st.NSFW || st.Protected {
		t.Fatalf("stat of the leaf: %+v", st)
	}
	if code, _ := u.setFlags("/top", `"protected":true`); code != 200 {
		t.Fatal("re-protect")
	}
	if st := u.statOf("/top/mid/leaf.txt"); !st.NSFW || !st.Protected {
		t.Fatalf("stat after protecting the ancestor: %+v", st)
	}
}

// Under a protected folder every delete, move-out, rename of the folder and overwrite
// is 403 with the reason, while a new upload, a folder and a move-in succeed.
//
// Invariant: I-163
func TestWebUI_Mutations_ProtectedIs403(t *testing.T) {
	u := newUI(t)
	u.write("/p/f", 0, []byte("f"))
	u.write("/p/d/g", 0, []byte("g"))
	u.write("/o/h", 0, []byte("h"))
	u.flush()
	if code, _ := u.setFlags("/p", `"protected":true`); code != 200 {
		t.Fatal("protect")
	}
	res := u.del("/p/f", "/p/d", "/p")
	if len(res.Deleted) != 0 || len(res.Failed) != 3 {
		t.Fatalf("delete under protection: %+v", res)
	}
	for _, f := range res.Failed {
		if f.Status != 403 || !strings.Contains(f.Error, "protected") {
			t.Fatalf("delete failure: %+v", f)
		}
	}
	var out batch
	if code := u.post("/api/move", `{"paths":["/p/f","/p/d/g"],"to":"/o"}`, &out); code != 200 || len(out.Moved) != 0 || out.Failed[0].Status != 403 || out.Failed[1].Status != 403 {
		t.Fatalf("move out: %d %+v", code, out)
	}
	if code := u.post("/api/rename", `{"path":"/p","name":"q"}`, nil); code != 403 {
		t.Fatalf("rename the protected folder: %d", code)
	}
	if code, _ := u.put("/p/f", []byte("replaced")); code != 403 {
		t.Fatalf("overwrite under protection: %d", code)
	}
	if string(u.readAt("/p/f", 0, 1)) != "f" || u.exists("/p/.f.tailraid-upload") {
		t.Fatal("the refused overwrite changed the file or left its temp")
	}
	if code, _ := u.put("/p/new", []byte("n")); code != 201 {
		t.Fatalf("a new file under protection: %d", code)
	}
	if code, _ := u.mkdir("/p/folder", webui.HeaderUI, "1"); code != 201 {
		t.Fatalf("a new folder under protection: %d", code)
	}
	if code := u.post("/api/move", `{"paths":["/o/h"],"to":"/p"}`, &out); code != 200 || len(out.Moved) != 1 {
		t.Fatalf("move in: %d %+v", code, out)
	}
	if code := u.post("/api/move", `{"paths":["/p/f"],"to":"/p/d"}`, &out); code != 200 || len(out.Moved) != 1 {
		t.Fatalf("move within the protected folder: %d %+v", code, out)
	}
}

// The pin routes keep a folder on this node: the folder's listing says pinned, the
// pin list carries it, an unpin releases it; the root and a file are 400.
//
// Invariant: I-162
func TestWebUI_Pins(t *testing.T) {
	var f *testharness.Fabric
	u := newUIWith(t, func(fab *testharness.Fabric) { f = fab }, func(o *webui.Options) {
		p, err := pins.Open(t.TempDir(), f.FSM, f.Reader(), f.Cache, f.Clk, nil)
		must(t, err)
		o.Pins = p
	})
	u.write("/music/a", 0, testharness.Pattern(100, 1))
	u.flush()
	var out map[string]any
	for body, want := range map[string]int{`{"path":"/","pinned":true}`: 400, `{"path":"/music/a","pinned":true}`: 400, `{"path":"/nope","pinned":true}`: 404, `{"path":"/.tailraid","pinned":true}`: 400} {
		if code := u.post("/api/pin", body, nil); code != want {
			t.Fatalf("%s: %d, want %d", body, code, want)
		}
	}
	if code := u.post("/api/pin", `{"path":"/music","pinned":true}`, &out); code != 200 || out["pinned"] != true || out["path"] != "/music" || out["bytes"] != float64(100) {
		t.Fatalf("pin: %d %v", code, out)
	}
	if l := u.lsOf("path=/music"); !l.Dir.Pinned {
		t.Fatalf("the listing does not say pinned: %+v", l.Dir)
	}
	var rep pins.Report
	code, _, body := u.get("/api/pins")
	must(t, json.Unmarshal(body, &rep))
	if code != 200 || len(rep.Pins) != 1 || rep.Pins[0].Path != "/music" || rep.Pins[0].Bytes != 100 {
		t.Fatalf("pins: %d %+v", code, rep)
	}
	if code := u.post("/api/pin", `{"path":"/music","pinned":false}`, &out); code != 200 || out["pinned"] != false {
		t.Fatalf("unpin: %d %v", code, out)
	}
	if l := u.lsOf("path=/music"); l.Dir.Pinned {
		t.Fatal("still pinned after the unpin")
	}
	if code := u.post("/api/pin", `{"path":"/music","pinned":false}`, nil); code != 404 {
		t.Fatalf("unpin again: %d", code)
	}
	// without pins wired the routes say so
	u2 := newUI(t)
	if code := u2.post("/api/pin", `{"path":"/","pinned":true}`, nil); code != 503 {
		t.Fatalf("pin without pins: %d", code)
	}
}
