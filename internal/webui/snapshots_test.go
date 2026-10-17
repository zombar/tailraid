package webui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
	"github.com/zombar/tailraid/internal/webui"
)

// fakeSnapshots is the node's snapshot service: the mirrors as given, and Take records
// what it was asked and answers err. A restore job it starts runs at once over fs as its
// user, its report kept in reports and its starter in jobs; a cancel follows the host's
// rule (the starter or an admin, once users exist) over the jobs it started.
type fakeSnapshots struct {
	mirrors []admin.MirrorReport
	took    []string
	err     error
	fs      *vfs.FS
	st      *meta.Store
	reports []vfs.RestoreReport
	jobs    map[string]meta.UserIdx
	cancels []string
}

func (f *fakeSnapshots) StartRestore(ctx context.Context, actor meta.UserIdx, mirror, snapshot, src, dst string) (rpc.RestoreStarted, error) {
	fs := f.fs
	if actor != 0 {
		fs = fs.As(actor)
	}
	vsrc := "/.snapshots/" + strings.ReplaceAll(mirror, "/", "-") + "/" + snapshot + strings.TrimSuffix(src, "/")
	rep, err := fs.RestoreSnapshot(ctx, vsrc, dst)
	if err != nil {
		return rpc.RestoreStarted{}, err
	}
	f.reports = append(f.reports, rep)
	if f.jobs == nil {
		f.jobs = map[string]meta.UserIdx{}
	}
	id := fmt.Sprintf("1-%d", len(f.jobs)+1)
	f.jobs[id] = actor
	return rpc.RestoreStarted{Job: id, Host: "one", Path: dst}, nil
}

func (f *fakeSnapshots) CancelRestore(_ context.Context, actor meta.UserIdx, job string) error {
	starter, ok := f.jobs[job]
	if !ok {
		return fmt.Errorf("%w: %s", rpc.ErrNoRestore, job)
	}
	if st := f.st; st.Enforcing() && actor != starter && !st.IsAdmin(actor) {
		return fmt.Errorf("cancel restore %s: %w", job, meta.ErrAccess)
	}
	f.cancels = append(f.cancels, job)
	return nil
}

func (f *fakeSnapshots) Mirrors() []admin.MirrorReport { return f.mirrors }

func (f *fakeSnapshots) Take(_ context.Context, name, mirror string) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.took = append(f.took, name+"@"+mirror)
	return []map[string]string{{"mirror": mirror, "name": name, "state": "syncing"}}, nil
}

func (f *fakeSnapshots) Schedule() webui.Schedule {
	return webui.Schedule{Every: time.Hour, Hourly: 24, Daily: 30, Weekly: 8, Monthly: 12, StaleAfter: 7 * 24 * time.Hour}
}

// snapUI is a web UI over a fabric whose volume has a mirror "host/m1" with snapshots.
type snapUI struct {
	*ui
	src *testharness.Mirrors
	m   meta.NodeIdx
	svc *fakeSnapshots
	cs  int
}

func newSnapUI(t *testing.T) *snapUI {
	t.Helper()
	svc := &fakeSnapshots{}
	u := newUI(t, func(o *webui.Options) { o.Snapshots = svc })
	s := &snapUI{ui: u, src: testharness.NewMirrors(), svc: svc, cs: int(u.f.FSM.Volume().ChunkSize)}
	svc.fs, svc.st = u.fs, u.f.FSM
	u.fs.SetFlusher(u.f.Flusher())
	must(t, u.fs.MkdirAll("/docs/sub", 0o755))
	s.m = u.f.AddMirror(s.src, "host/m1")
	u.fs.SetMirrors(s.src)
	return s
}

// put makes name hold data exactly: written at a non-zero offset after its head, and
// flushed.
func (s *snapUI) put(name string, data []byte) {
	s.t.Helper()
	fh, err := s.fs.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	must(s.t, err)
	half := len(data) / 2
	for _, w := range []struct {
		off int
		b   []byte
	}{{half, data[half:]}, {0, data[:half]}} {
		_, err = fh.Seek(int64(w.off), io.SeekStart)
		must(s.t, err)
		_, err = fh.Write(w.b)
		must(s.t, err)
	}
	must(s.t, fh.Close())
	s.flush()
}

func (s *snapUI) take(name string) meta.Snapshot {
	s.t.Helper()
	s.f.Clk.Advance(time.Minute)
	return s.f.TakeSnapshot(s.src, s.m, name)
}

func (s *snapUI) body(path string) []byte {
	s.t.Helper()
	code, _, b := s.get(path)
	if code != http.StatusOK {
		s.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	return b
}

// GET /api/snapshots lists every mirror (its /.snapshots directory, state from the
// probes, why its card warns) and every snapshot newest first, an offline mirror's
// included and a deleting one left out, with the schedule; POST /api/snapshot passes the
// name and the mirror's row on to the node and answers 201, 409 offline, 409 taken and
// 503 too old; no route removes a snapshot (docs/13 §3, I-183).
//
// Invariant: I-187, I-183
func TestWebUI_Snapshots_ListTake_NoDeleteRoute(t *testing.T) {
	s := newSnapUI(t)
	s.put("/docs/a.txt", []byte("first"))
	first := s.take("2026-10-12-0900Z")
	s.put("/docs/a.txt", []byte("second"))
	second := s.take("before-reorg")
	gone := s.take("thinned")
	s.f.MustApply(meta.Command{Op: meta.OpSnapshotDelete, SnapshotDelete: &meta.SnapshotDelete{ID: gone.ID, Now: s.f.Clk.Now().UnixNano()}})
	s.svc.mirrors = []admin.MirrorReport{
		{
			Idx: s.m, Name: "host/m1", Host: "one", Self: true, Path: "/Volumes/A", Online: true, Used: 10, Free: 1 << 20, Total: 1 << 40,
			Syncing: &admin.SyncReport{ID: 9, Snapshot: "now", Read: 1, Total: 4, BytesNew: 2},
		},
		{Self: true, Path: "/Volumes/Unregistered"},
	}

	var got struct {
		Mirrors   []webui.MirrorJSON   `json:"mirrors"`
		Snapshots []webui.SnapshotJSON `json:"snapshots"`
		Schedule  webui.ScheduleJSON   `json:"schedule"`
	}
	must(t, json.Unmarshal(s.body("/api/snapshots"), &got))
	if len(got.Mirrors) != 1 || got.Mirrors[0].Name != "host-m1" || !got.Mirrors[0].Online || got.Mirrors[0].Syncing == nil || got.Mirrors[0].Syncing.BytesNew != 2 || got.Mirrors[0].Warn != "reserve" {
		t.Errorf("mirrors = %+v, want host-m1 online, syncing, under its reserve", got.Mirrors)
	}
	if len(got.Snapshots) != 2 || got.Snapshots[0].ID != uint32(second.ID) || got.Snapshots[1].ID != uint32(first.ID) || got.Snapshots[0].Mirror != "host-m1" || got.Snapshots[0].State != "complete" {
		t.Errorf("snapshots = %+v, want before-reorg then the first, the deleting one left out", got.Snapshots)
	}
	if got.Schedule.Every != 3600 || got.Schedule.Hourly != 24 || got.Schedule.Next == "" {
		t.Errorf("schedule = %+v", got.Schedule)
	}

	// an offline mirror's snapshots stay listed (docs/16 §5)
	s.src.SetOnline(s.m, false)
	s.svc.mirrors[0].Online, s.svc.mirrors[0].Syncing = false, nil
	must(t, json.Unmarshal(s.body("/api/snapshots"), &got))
	if len(got.Snapshots) != 2 || got.Mirrors[0].Online {
		t.Errorf("offline: %+v", got)
	}
	s.src.SetOnline(s.m, true)

	for _, tc := range []struct {
		body, want string
		err        error
		code       int
		offline    bool
	}{
		{`{}`, "@", nil, 201, false},
		{`{"name":"mine","mirror":"host-m1"}`, "mine@host/m1", nil, 201, false},
		{`{"name":"x","mirror":"nope-m9"}`, "", nil, 404, false},
		{`{"name":"a/b"}`, "", nil, 400, false},
		{`{"name":"` + strings.Repeat("n", meta.MaxSnapName+1) + `"}`, "", nil, 400, false},
		{`{"name":"` + strings.Repeat("n", meta.MaxSnapName) + `"}`, strings.Repeat("n", meta.MaxSnapName) + "@", nil, 201, false},
		{`{}`, "", fmt.Errorf("mirror host/m1: %w", vfs.ErrMirrorOffline), 409, true},
		{`{}`, "", fmt.Errorf("syncing: %w", meta.ErrExist), 409, false},
		{`{}`, "", fmt.Errorf("old member: %w", meta.ErrCodec), 503, false},
	} {
		s.svc.err, s.svc.took = tc.err, nil
		var reply map[string]any
		var rows []map[string]any
		code := s.post("/api/snapshot", tc.body, nil)
		if code == 201 {
			s.post("/api/snapshot", tc.body, &rows)
		} else {
			s.post("/api/snapshot", tc.body, &reply)
		}
		if code != tc.code {
			t.Errorf("%s (err %v): %d, want %d", tc.body, tc.err, code, tc.code)
		}
		if tc.want != "" && (len(s.svc.took) == 0 || s.svc.took[0] != tc.want) {
			t.Errorf("%s: took %v, want %s", tc.body, s.svc.took, tc.want)
		}
		if tc.offline && reply["offline"] != true {
			t.Errorf("%s: %v, want offline:true", tc.body, reply)
		}
	}
	s.svc.err = nil
	if resp := s.do(http.MethodPost, "/api/snapshot", strings.NewReader(`{}`)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a snapshot without the UI header: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// no route proposes SnapshotDelete (I-183)
	before := len(s.f.FSM.Snapshots())
	id := fmt.Sprint(first.ID)
	for _, rt := range []struct{ method, path string }{
		{http.MethodDelete, "/api/snapshots/" + id},
		{http.MethodDelete, "/api/snapshot?id=" + id},
		{http.MethodDelete, "/api/snapshots?id=" + id},
		{http.MethodPost, "/api/snapshot-delete"},
		{http.MethodPost, "/api/snapshots/delete"},
		{http.MethodPost, "/api/delete-snapshot"},
		{http.MethodPost, "/api/delete?at=" + id},
		{http.MethodPost, "/api/purge?at=" + id},
		{http.MethodPost, "/api/snapshots"},
	} {
		resp := s.do(rt.method, rt.path, strings.NewReader(`{"id":`+id+`,"snapshot":`+id+`,"paths":["/"],"all":true}`), webui.HeaderUI, "1", "Content-Type", "application/json")
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s %s answered %d", rt.method, rt.path, resp.StatusCode)
		}
	}
	snaps := s.f.FSM.Snapshots()
	if len(snaps) != before {
		t.Fatalf("the catalogue changed: %d rows, had %d", len(snaps), before)
	}
	for _, sn := range snaps {
		if sn.ID == first.ID && sn.State != meta.SnapComplete {
			t.Fatalf("snapshot %d is %v", first.ID, sn.State)
		}
	}
}

// ls, stat, files, tags, thumb and render read as of the snapshot at= names: its bytes,
// not the live ones, dir carrying the snapshot; every mutating route given at= is 400, in=
// with at= 400, an unknown snapshot 404 (docs/13 §3).
//
// Invariant: I-187
func TestWebUI_LsAt_ReadOnly(t *testing.T) {
	s := newSnapUI(t)
	old := testharness.Pattern(2*s.cs+s.cs/3, 3)
	s.put("/docs/a.bin", old)
	s.put("/docs/sub/note.md", []byte("# then\n"))
	sn := s.take("s1")
	s.put("/docs/a.bin", testharness.Pattern(s.cs, 9))
	s.put("/docs/sub/note.md", []byte("# now\n"))
	s.put("/docs/new.txt", []byte("not in the snapshot"))
	at := fmt.Sprintf("&at=%d", sn.ID)

	l := s.lsOf("path=/docs" + at)
	if l.Dir.Snapshot == nil || l.Dir.Snapshot.Name != "s1" || l.Dir.Snapshot.Mirror != "host-m1" {
		t.Fatalf("dir = %+v, want the snapshot", l.Dir)
	}
	names := map[string]webui.Entry{}
	for _, e := range l.Entries {
		names[e.Name] = e
	}
	if _, ok := names["new.txt"]; ok || names["a.bin"].Size != uint64(len(old)) || names["sub"].Type != "dir" || len(names) != 2 {
		t.Fatalf("entries as of s1 = %+v", l.Entries)
	}
	if root := s.lsOf("path=/" + at); len(root.Entries) != 1 || root.Entries[0].Name != "docs" {
		t.Fatalf("root as of s1 = %+v", root.Entries)
	}
	var st webui.Entry
	must(t, json.Unmarshal(s.body("/api/stat?path=/docs/a.bin"+at), &st))
	if st.Size != uint64(len(old)) || st.Path != "/docs/a.bin" || st.Dirty {
		t.Fatalf("stat as of s1 = %+v", st)
	}
	if b := s.body("/files/docs/a.bin?" + at[1:]); !bytes.Equal(b, old) {
		t.Fatalf("GET as of s1: %d bytes, want the snapshot's %d", len(b), len(old))
	}
	code, hdr, part := s.get("/files/docs/a.bin?"+at[1:], "Range", fmt.Sprintf("bytes=%d-%d", s.cs-1, s.cs))
	if code != http.StatusPartialContent || !bytes.Equal(part, old[s.cs-1:s.cs+1]) || hdr.Get("ETag") == "" {
		t.Fatalf("range as of s1: %d %q", code, part)
	}
	if b := s.body("/api/render?path=/docs/sub/note.md" + at); !strings.Contains(string(b), "then") {
		t.Fatalf("render as of s1: %s", b)
	}
	if code, _, _ := s.get("/api/thumb?path=/docs/a.bin" + at); code != http.StatusNotFound {
		t.Errorf("thumb of a non-image as of s1: %d", code)
	}
	for _, tc := range []struct {
		q    string
		code int
	}{
		{"path=/docs&at=999", 404}, {"path=/docs&at=x", 400}, {"path=/docs&at=0", 400}, {"path=/docs&in=5" + at, 400}, {"path=/docs/new.txt" + at, 404},
	} {
		if code, _, b := s.get("/api/ls?" + tc.q); code != tc.code {
			t.Errorf("ls?%s: %d %s, want %d", tc.q, code, b, tc.code)
		}
	}
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/mkdir?" + at[1:], `{"path":"/docs/x"}`},
		{http.MethodPost, "/api/delete?" + at[1:], `{"paths":["/docs/a.bin"]}`},
		{http.MethodPost, "/api/rename?" + at[1:], `{"path":"/docs/a.bin","name":"b"}`},
		{http.MethodPost, "/api/revert?" + at[1:], fmt.Sprintf(`{"path":"/docs/a.bin","snapshot":%d}`, sn.ID)},
		{http.MethodPut, "/files/docs/a.bin?" + at[1:], "x"},
	} {
		resp := s.do(rt.method, rt.path, strings.NewReader(rt.body), webui.HeaderUI, "1", "Content-Type", "application/json")
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s: %d, want 400", rt.method, rt.path, resp.StatusCode)
		}
	}
	if got := s.readAt("/docs/a.bin", 0, int64(s.cs)); !bytes.Equal(got, testharness.Pattern(s.cs, 9)) {
		t.Fatal("a refused mutation changed the live file")
	}
}

// A snapshot whose mirror is offline answers 409 {"offline":true} on every route that
// would read it, never stale or partial bytes (docs/13 §3).
//
// Invariant: I-187
func TestWebUI_OfflineMirror_409(t *testing.T) {
	s := newSnapUI(t)
	s.put("/docs/a.bin", testharness.Pattern(s.cs+1, 4))
	sn := s.take("s1")
	s.src.SetOnline(s.m, false)
	at := fmt.Sprintf("at=%d", sn.ID)
	for _, p := range []string{"/api/ls?path=/docs&" + at, "/api/stat?path=/docs/a.bin&" + at, "/files/docs/a.bin?" + at, "/api/thumb?path=/docs/a.bin&" + at, "/api/tags?path=/docs/a.bin&" + at} {
		code, _, b := s.get(p)
		var reply map[string]any
		_ = json.Unmarshal(b, &reply)
		if code != http.StatusConflict || reply["offline"] != true {
			t.Errorf("GET %s: %d %s, want 409 offline", p, code, b)
		}
	}
	for _, rt := range []struct{ route, body string }{
		{"/api/revert", fmt.Sprintf(`{"path":"/docs/a.bin","snapshot":%d}`, sn.ID)},
		{"/api/copy", fmt.Sprintf(`{"path":"/docs/a.bin","snapshot":%d,"name":"c"}`, sn.ID)},
		{"/api/restore-tree", fmt.Sprintf(`{"path":"/docs","snapshot":%d,"name":"r"}`, sn.ID)},
	} {
		var reply map[string]any
		if code := s.post(rt.route, rt.body, &reply); code != http.StatusConflict || reply["offline"] != true {
			t.Errorf("%s: %d %v, want 409 offline", rt.route, code, reply)
		}
	}
}

// GET /api/versions lists the file's distinct contents across the online mirrors, the
// live one first and current with what holds it (or nothing), the others newest first
// with every snapshot holding them, and counts an offline mirror's snapshots (docs/16 §6).
//
// Invariant: I-187
func TestWebUI_Versions_DistinctContents(t *testing.T) {
	s := newSnapUI(t)
	v1 := testharness.Pattern(s.cs+5, 1)
	s.put("/docs/a.bin", v1)
	s1 := s.take("s1")
	s2 := s.take("s2") // the same content: one version held by two snapshots
	s.put("/docs/a.bin", testharness.Pattern(s.cs+5, 2))
	s3 := s.take("s3")
	s.put("/docs/a.bin", testharness.Pattern(7, 3)) // in no snapshot yet

	other := s.f.AddMirror(s.src, "host/m2")
	s.f.TakeSnapshot(s.src, other, "away-1")
	s.f.TakeSnapshot(s.src, other, "away-2")
	s.src.SetOnline(other, false)

	var got struct {
		Versions []webui.VersionJSON `json:"versions"`
		Offline  []struct {
			Mirror string `json:"mirror"`
			Count  int    `json:"count"`
		} `json:"offline"`
	}
	must(t, json.Unmarshal(s.body("/api/versions?path="+url.QueryEscape("/docs/a.bin")), &got))
	if len(got.Versions) != 3 {
		t.Fatalf("%d versions, want current, s3, s1+s2: %+v", len(got.Versions), got.Versions)
	}
	cur, mid, oldest := got.Versions[0], got.Versions[1], got.Versions[2]
	if !cur.Current || cur.Size != 7 || len(cur.Snapshots) != 0 || cur.Key != "now" {
		t.Errorf("current = %+v", cur)
	}
	if mid.Current || len(mid.Snapshots) != 1 || mid.Snapshots[0].ID != uint32(s3.ID) || mid.Key != fmt.Sprint(s3.ID) {
		t.Errorf("middle = %+v", mid)
	}
	if len(oldest.Snapshots) != 2 || oldest.Snapshots[0].ID != uint32(s2.ID) || oldest.Snapshots[1].ID != uint32(s1.ID) || oldest.Size != uint64(len(v1)) || oldest.Snapshots[0].Mirror != "host-m1" {
		t.Errorf("oldest = %+v", oldest)
	}
	if len(got.Offline) != 1 || got.Offline[0].Mirror != "host-m2" || got.Offline[0].Count != 2 {
		t.Errorf("offline = %+v", got.Offline)
	}
	for _, q := range []string{"/", "/docs", "/.tailraid/x", "/missing"} {
		if code, _, _ := s.get("/api/versions?path=" + url.QueryEscape(q)); code < 400 {
			t.Errorf("versions of %s: %d", q, code)
		}
	}
}

// Revert makes the snapshot's bytes current and counts the chunks copied back from the
// mirror; copy writes them beside the file; restore-tree starts the job that recreates a
// folder, skipping what exists, and answers 202; nothing on the mirror changes (docs/13
// §3, docs/16 §6, I-186).
//
// Invariant: I-187, I-186, I-369
func TestWebUI_Revert_Copy_RestoreTree(t *testing.T) {
	s := newSnapUI(t)
	old := testharness.Pattern(2*s.cs+s.cs/2, 5)
	s.put("/docs/a.bin", old)
	s.put("/docs/sub/b.txt", []byte("bee"))
	sn := s.take("s1")
	newer := testharness.Pattern(s.cs+3, 6)
	s.put("/docs/a.bin", newer)
	objs := s.src.Objects()
	used := s.src.Mirror(s.m).Used()

	var rev struct {
		Path     string `json:"path"`
		Uploaded int    `json:"uploaded"`
	}
	if code := s.post("/api/revert", fmt.Sprintf(`{"path":"/docs/a.bin","snapshot":%d}`, sn.ID), &rev); code != 200 || rev.Uploaded != 0 {
		t.Fatalf("revert: %d %+v, want 200 and nothing uploaded while the pool holds the chunks", code, rev)
	}
	if got := s.readAt("/docs/a.bin", 0, int64(len(old)+1)); !bytes.Equal(got, old) {
		t.Fatalf("after the revert: %d bytes, want the snapshot's %d", len(got), len(old))
	}
	if s.src.Objects() != objs {
		t.Errorf("a revert from the pool read %d objects from the mirror", s.src.Objects()-objs)
	}

	var cp struct {
		Entry webui.Entry `json:"entry"`
	}
	s.put("/docs/a.bin", newer)
	if code := s.post("/api/copy", fmt.Sprintf(`{"path":"/docs/a.bin","snapshot":%d,"name":"a (s1).bin"}`, sn.ID), &cp); code != 201 || cp.Entry.Path != "/docs/a (s1).bin" {
		t.Fatalf("copy: %d %+v", code, cp)
	}
	if got := s.readAt("/docs/a (s1).bin", 0, int64(len(old)+1)); !bytes.Equal(got, old) {
		t.Fatal("the copy does not hold the snapshot's bytes")
	}
	if got := s.readAt("/docs/a.bin", 0, int64(len(newer)+1)); !bytes.Equal(got, newer) {
		t.Fatal("a copy changed the file beside it")
	}
	if code := s.post("/api/copy", fmt.Sprintf(`{"path":"/docs/a.bin","snapshot":%d,"name":"a (s1).bin"}`, sn.ID), nil); code != http.StatusConflict {
		t.Errorf("copy onto a taken name: %d, want 409", code)
	}
	for _, name := range []string{"", "x/y", ".."} {
		if code := s.post("/api/copy", fmt.Sprintf(`{"path":"/docs/a.bin","snapshot":%d,"name":%q}`, sn.ID, name), nil); code != http.StatusBadRequest {
			t.Errorf("copy as %q: %d, want 400", name, code)
		}
	}

	var tree struct {
		Job  string `json:"job"`
		Host string `json:"host"`
		Path string `json:"path"`
	}
	if code := s.post("/api/restore-tree", fmt.Sprintf(`{"path":"/docs","snapshot":%d,"to":"/","name":"restored"}`, sn.ID), &tree); code != http.StatusAccepted || tree.Path != "/restored" || tree.Job == "" {
		t.Fatalf("restore-tree: %d %+v", code, tree)
	}
	if rep := s.svc.reports[len(s.svc.reports)-1]; rep.Files != 2 || len(rep.Skipped) != 0 {
		t.Fatalf("the job's restore %+v", rep)
	}
	if got := s.readAt("/restored/sub/b.txt", 0, 10); string(got) != "bee" {
		t.Fatalf("restored b.txt = %q", got)
	}
	must(t, s.fs.Remove("/restored/sub/b.txt"))
	if code := s.post("/api/restore-tree", fmt.Sprintf(`{"path":"/docs","snapshot":%d,"to":"/","name":"restored"}`, sn.ID), &tree); code != http.StatusAccepted {
		t.Fatalf("second restore-tree: %d %+v", code, tree)
	}
	if rep := s.svc.reports[len(s.svc.reports)-1]; rep.Files != 1 || len(rep.Skipped) != 1 {
		t.Fatalf("second restore %+v, want the missing file made and the other skipped", rep)
	}
	if code := s.post("/api/restore-tree", fmt.Sprintf(`{"path":"/docs","snapshot":%d,"to":"/.tailraid"}`, sn.ID), nil); code < 400 {
		t.Errorf("restore into /.tailraid: %d", code)
	}
	if s.src.Mirror(s.m).Used() != used {
		t.Error("a restore changed the mirror")
	}
}

// A mirror's card warns `incomplete` only while its newest finished snapshot is
// incomplete: a later complete one ends the warning, the older one keeping its missing
// count in the table (docs/16 §5, I-392).
// Invariant: I-392
func TestWebUI_MirrorWarnsOnlyForNewestIncomplete(t *testing.T) {
	s := newSnapUI(t)
	s.svc.mirrors = []admin.MirrorReport{{Idx: s.m, Name: "host/m1", Host: "one", Self: true, Online: true, Free: 1 << 40, Total: 1 << 40}}
	row := func(name string, missing uint32) {
		s.f.Clk.Advance(time.Minute)
		now := s.f.Clk.Now().UnixNano()
		id := s.f.MustApply(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: s.m, Name: name, Now: now}}).SnapID
		s.f.MustApply(meta.Command{Op: meta.OpSnapshotEnd, SnapshotEnd: &meta.SnapshotEnd{ID: id, OK: true, Missing: missing, Now: now}})
	}
	var got struct {
		Mirrors   []webui.MirrorJSON   `json:"mirrors"`
		Snapshots []webui.SnapshotJSON `json:"snapshots"`
	}
	row("old", 2)
	must(t, json.Unmarshal(s.body("/api/snapshots"), &got))
	if len(got.Mirrors) != 1 || got.Mirrors[0].Warn != "incomplete" {
		t.Fatalf("the newest snapshot is incomplete: %+v", got.Mirrors)
	}
	row("new", 0)
	got.Mirrors, got.Snapshots = nil, nil // warn is omitted when empty
	must(t, json.Unmarshal(s.body("/api/snapshots"), &got))
	if got.Mirrors[0].Warn != "" {
		t.Fatalf("a complete snapshot came since: the card warns %q", got.Mirrors[0].Warn)
	}
	if len(got.Snapshots) != 2 || got.Snapshots[1].Name != "old" || got.Snapshots[1].State != "incomplete" || got.Snapshots[1].Missing != 2 {
		t.Fatalf("the older snapshot keeps its count in the table: %+v", got.Snapshots)
	}
}
