package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// node is another web UI over the same volume with nothing resolved in memory: another
// node of the cluster, or this one after a restart.
func (u *ui) node() *ui {
	u.t.Helper()
	srv, err := webui.New(webui.Options{FS: u.fs, Store: u.f.FSM, Auth: u.auth, Login: login, Clk: u.f.Clk, Logger: slog.New(slog.DiscardHandler)})
	must(u.t, err)
	v := &ui{t: u.t, f: u.f, fs: u.fs, ts: u.ts, auth: u.auth, srv: srv, client: u.client}
	v.member.Store(true)
	v.hs = httptest.NewServer(srv.Handler())
	u.t.Cleanup(v.hs.Close)
	return v
}

func (u *ui) coverRecord(dir string) (string, bool) {
	u.t.Helper()
	in, err := u.f.FSM.Resolve(meta.SplitPath(dir))
	must(u.t, err)
	f, err := u.fs.Open(path.Join(webui.ThumbDir, fmt.Sprintf("%d-%d.cover", in.Ino, in.Gen)))
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	must(u.t, err)
	var rec struct {
		Name string `json:"name"`
	}
	must(u.t, json.Unmarshal(b, &rec))
	return rec.Name, true
}

// A folder's cover, once one node resolved it, is stored in the volume beside the
// thumbnails, and every other node (or this one after a restart) serves it from that
// record without reading a track's tags; the record names the folder versions it was read
// from, so a change to one makes the next request resolve it again and store the new
// choice. Before, the choice lived in each node's memory only, and every node resolved
// every folder again, reading tags over the network, after every restart.
// Invariant: I-256
func TestWebUI_DirCover_StoredAndShared(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(16, 16, green)
	audio := testharness.Pattern(500, 1)
	u.write("/alb/a.mp3", 0, append(id3v2(map[string]string{"TIT2": "a"}, nil), audio...))
	u.write("/alb/b.mp3", 0, append(id3v2(map[string]string{"TIT2": "b"}, pic), audio...))
	u.write("/alb/c.mp3", 0, append(id3v2(map[string]string{"TIT2": "c"}, pic), audio...))
	u.flush()

	if code, _, _ := u.get("/api/thumb?path=/alb"); code != 200 {
		t.Fatalf("folder thumb on the first node: %d", code)
	}
	u.flush()
	if name, ok := u.coverRecord("/alb"); !ok || name != "b.mp3" {
		t.Fatalf("the cover record after the first node resolved /alb: %q (%v), want b.mp3", name, ok)
	}

	// another node serves the stored choice: plant a record that names c.mp3, which tag
	// reading would not pick, and that node answers c.mp3
	in, err := u.f.FSM.Resolve(meta.SplitPath("/alb"))
	must(t, err)
	planted, err := json.Marshal(map[string]any{"name": "c.mp3", "deps": []map[string]any{{"ino": in.Ino, "version": in.Version, "mtime": in.Mtime}}})
	must(t, err)
	fh, err := u.fs.OpenFile(path.Join(webui.ThumbDir, fmt.Sprintf("%d-%d.cover", in.Ino, in.Gen)), os.O_WRONLY|os.O_TRUNC, 0o644)
	must(t, err)
	_, err = fh.Write(planted)
	must(t, err)
	must(t, fh.Close())
	u.flush()
	other := u.node()
	if code, _, _ := other.get("/api/thumb?path=/alb"); code != 200 {
		t.Fatalf("folder thumb on another node: %d", code)
	}
	if l := other.lsOf("path=/alb"); l.Dir.Cover != "c.mp3" {
		t.Fatalf("another node's cover of /alb: %q, want the stored c.mp3 (it resolved the folder again)", l.Dir.Cover)
	}

	// a change to the folder makes the record stale: the next request resolves and stores
	u.write("/alb/0.mp3", 0, append(id3v2(map[string]string{"TIT2": "0"}, pic), audio...))
	u.flush()
	third := u.node()
	if code, _, _ := third.get("/api/thumb?path=/alb"); code != 200 {
		t.Fatal("folder thumb after the change")
	}
	u.flush()
	if l := third.lsOf("path=/alb"); l.Dir.Cover != "0.mp3" {
		t.Fatalf("cover after adding 0.mp3: %q, want 0.mp3", l.Dir.Cover)
	}
	if name, _ := u.coverRecord("/alb"); name != "0.mp3" {
		t.Fatalf("the record after the change: %q, want 0.mp3", name)
	}
}

// The thumbnail sweep keeps the cover record of a folder that exists (live or in the
// trash) and removes one whose folder is gone.
// Invariant: I-256
func TestSweepThumbs_CoverRecords(t *testing.T) {
	u := newUI(t)
	pic := jpegBytes(16, 16, green)
	u.write("/alb/b.mp3", 0, append(id3v2(map[string]string{"TIT2": "b"}, pic), testharness.Pattern(500, 1)...))
	u.flush()
	if code, _, _ := u.get("/api/thumb?path=/alb"); code != 200 {
		t.Fatal("folder thumb")
	}
	gone := path.Join(webui.ThumbDir, "999999-1.cover")
	fh, err := u.fs.OpenFile(gone, os.O_WRONLY|os.O_CREATE, 0o644)
	must(t, err)
	_, err = fh.Write([]byte(`{"name":"x.mp3"}`))
	must(t, err)
	must(t, fh.Close())
	u.flush()
	if _, err := webui.SweepThumbs(context.Background(), u.fs, u.f.FSM); err != nil {
		t.Fatal(err)
	}
	u.flush()
	names, err := u.fs.ReadDir(webui.ThumbDir)
	must(t, err)
	var got []string
	for _, n := range names {
		got = append(got, n.Name())
	}
	if slices.Contains(got, "999999-1.cover") {
		t.Errorf("the record of a folder that does not exist survived the sweep: %v", got)
	}
	if _, ok := u.coverRecord("/alb"); !ok {
		t.Errorf("the sweep removed the record of a live folder: %v", got)
	}
}
