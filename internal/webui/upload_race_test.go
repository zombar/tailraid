package webui_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
	"github.com/zombar/tailraid/internal/webui"
)

// otherUI is the browser UI of a second gateway over u's fabric: its own overlay and
// filesystem (another device's tailraid), its own node name, the same authorizer.
func otherUI(t *testing.T, u *ui, node string) *ui {
	t.Helper()
	fs, fl := u.f.OtherGateway()
	o := &ui{t: t, f: u.f, fs: fs, ts: u.ts, auth: u.auth, client: u.client}
	otherFlushers[o] = fl
	o.member.Store(true)
	srv, err := webui.New(webui.Options{FS: fs, Store: u.f.FSM, Auth: u.auth, Login: login, Clk: u.f.Clk, Logger: slog.New(slog.DiscardHandler), Node: node})
	must(t, err)
	o.srv = srv
	o.hs = httptest.NewServer(srv.Handler())
	t.Cleanup(o.hs.Close)
	t.Cleanup(func() { delete(otherFlushers, o) })
	return o
}

// otherFlushers are the second gateways' flushers, by their UI.
var otherFlushers = map[*ui]*vfs.Flusher{}

// flushBoth commits both gateways' dirty data, as their flushers would within seconds.
func flushBoth(t *testing.T, a, b *ui) {
	t.Helper()
	must(t, a.f.Flusher().FlushAll(ctx))
	must(t, otherFlushers[b].FlushAll(ctx))
}

func (u *ui) read(path string) []byte {
	u.t.Helper()
	f, err := u.fs.Open(path)
	must(u.t, err)
	defer f.Close()
	b, err := io.ReadAll(f)
	must(u.t, err)
	return b
}

// Two browsers upload one name in parts through two gateways at once, their parts
// interleaved: each gateway writes its own temp, both uploads succeed, and the name
// holds exactly the upload that published last, whole.
// Invariant: I-226
func TestWebUI_Put_TwoGateways_OneName_OneWhole(t *testing.T) {
	a := newUI(t, func(o *webui.Options) { o.Node = "one" })
	b := otherUI(t, a, "two")
	cs := int(testharness.ChunkSize)
	pa, pb := testharness.Pattern(2*cs+10, 1), testharness.Pattern(2*cs+10, 2)
	half := cs + 5
	if code, _ := a.putPart("/same.bin", pa[:half], 0, len(pa)); code != http.StatusAccepted {
		t.Fatalf("A's first part: %d", code)
	}
	if code, out := b.putPart("/same.bin", pb[:half], 0, len(pb)); code != http.StatusAccepted {
		t.Fatalf("B's first part: %d %v (the gateways share a temp)", code, out)
	}
	if code, _ := a.putPart("/same.bin", pa[half:], half, len(pa)); code != http.StatusCreated {
		t.Fatalf("A's last part: %d", code)
	}
	if code, _ := b.putPart("/same.bin", pb[half:], half, len(pb)); code != http.StatusNoContent {
		t.Fatalf("B's last part: %d", code)
	}
	flushBoth(t, a, b)
	for _, u := range []*ui{a, b} {
		if got := u.read("/same.bin"); !bytes.Equal(got, pb) {
			t.Fatalf("the name holds %d bytes that are not B's upload whole (differs at %d)", len(got), testharness.FirstDiff(got, pb))
		}
	}
	for _, name := range []string{".same.bin.one.tailraid-upload", ".same.bin.two.tailraid-upload"} {
		if a.exists("/" + name) {
			t.Fatalf("temp %s left behind", name)
		}
	}
}

// Two exclusive uploads (If-None-Match: *) of one name through two gateways: the one
// whose publish lands second, after the other's passed every check this gateway could
// make, gets 412 and its temp goes; the name holds the first upload whole.
// Invariant: I-226
func TestWebUI_Put_TwoGateways_IfNoneMatch_One412(t *testing.T) {
	a := newUI(t, func(o *webui.Options) { o.Node = "one" })
	b := otherUI(t, a, "two")
	pa, pb := []byte("A's upload"), []byte("B's upload, longer")
	// B's publish lands just before A's own rename reaches the leader
	a.f.ForestallNext(meta.OpRename, func() {
		resp := b.do(http.MethodPut, "/files/new.txt", bytes.NewReader(pb), webui.HeaderUI, "1", "If-None-Match", "*")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("B's exclusive upload: %d", resp.StatusCode)
		}
	})
	resp := a.do(http.MethodPut, "/files/new.txt", bytes.NewReader(pa), webui.HeaderUI, "1", "If-None-Match", "*")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("A's exclusive upload over B's: %d, want 412", resp.StatusCode)
	}
	flushBoth(t, a, b)
	if got := a.read("/new.txt"); !bytes.Equal(got, pb) {
		t.Fatalf("the name holds %q, want B's upload", got)
	}
	if a.exists("/.new.txt.one.tailraid-upload") {
		t.Fatal("A's temp was left behind")
	}
}
