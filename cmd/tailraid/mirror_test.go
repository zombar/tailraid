package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// mirrorNode is one initialised node serving in-process, with doc written into the
// volume through the browser UI's upload route.
type mirrorNode struct {
	t    *testing.T
	path string // the config
	home string
	cfg  config.Config
	cid  [16]byte
	stop func()
}

const mirrorDoc = "the document, as it was when the snapshot was taken"

func startMirrorNode(t *testing.T) *mirrorNode {
	t.Helper()
	path, cfg := devHome(t)
	cfg.Node.Disks = []config.Disk{{Path: filepath.Join(filepath.Dir(path), "d1"), Capacity: "1G"}} // a volume with no disk holds nothing
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "init", "--config", path, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	n := &mirrorNode{t: t, path: path, home: filepath.Dir(path), cfg: cfg}
	var err error
	if n.cid, err = cluster.LoadClusterID(cfg.Node.DataDir); err != nil {
		t.Fatal(err)
	}
	n.serve()
	t.Cleanup(func() { n.stop() })
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/files/doc", cfg.Listen.Web), strings.NewReader(mirrorDoc))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(mirrorDoc))
	req.Header.Set(webui.HeaderUI, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %s", resp.Status)
	}
	// a snapshot holds committed bytes only (docs/16 §3): wait for the flusher
	testharness.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/stat?path=/doc", cfg.Listen.Web))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var st struct {
			Size  int64 `json:"size"`
			Dirty bool  `json:"dirty"`
		}
		return json.NewDecoder(resp.Body).Decode(&st) == nil && !st.Dirty && st.Size == int64(len(mirrorDoc))
	}, "doc flushed")
	return n
}

// serve starts the node from the config file as it is now (mirror add rewrites it).
func (n *mirrorNode) serve() {
	n.t.Helper()
	cfg, err := config.Load(n.path)
	if err != nil {
		n.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- node.Serve(ctx, cfg, slog.New(slog.DiscardHandler), "test") }()
	stopped := false
	n.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			n.t.Errorf("serve: %v", err)
		}
	}
	waitReady(ctx, n.t, fmt.Sprintf("127.0.0.1:%d", n.cfg.Listen.RPC), "node never became ready")
}

func (n *mirrorNode) run(args ...string) (string, error) {
	n.t.Helper()
	return run(n.t, append(args, "--config", n.path)...)
}

func (n *mirrorNode) must(args ...string) string {
	n.t.Helper()
	out, err := n.run(args...)
	if err != nil {
		n.t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

// snapshots is `snapshot ls --json`.
func (n *mirrorNode) snapshots() []node.SnapshotInfo {
	n.t.Helper()
	var rows []node.SnapshotInfo
	if err := json.Unmarshal([]byte(n.must("snapshot", "ls", "--json")), &rows); err != nil {
		n.t.Fatal(err)
	}
	return rows
}

// settled waits until no snapshot is syncing and returns the catalogue by name.
func (n *mirrorNode) settled(want int) map[string]node.SnapshotInfo {
	n.t.Helper()
	var by map[string]node.SnapshotInfo
	testharness.Eventually(n.t, func() bool {
		by = map[string]node.SnapshotInfo{}
		for _, r := range n.snapshots() {
			if r.State == "syncing" {
				return false
			}
			by[r.Name] = r
		}
		return len(by) == want
	}, fmt.Sprintf("%d settled snapshots", want))
	return by
}

func (n *mirrorNode) mirrorName() string {
	host, _ := os.Hostname()
	return host + "/m1"
}

// `mirror add` marks an empty path, registers it, opens it at once (the schedule's first
// snapshot begins without a restart or a minute's wait), refuses a non-empty path and a
// mirror of another cluster, keeps the row's name on a retry, and with --adopt takes a
// foreign mirror over and imports its snapshots (docs/16 §1, §5, §7).
// Invariant: I-188
func TestCLI_MirrorAdd_MarkRehomeAdopt(t *testing.T) {
	n := startMirrorNode(t)
	junk := filepath.Join(n.home, "junk")
	_ = os.MkdirAll(junk, 0o700)
	_ = os.WriteFile(filepath.Join(junk, "file"), []byte("x"), 0o600)
	if out, err := n.run("mirror", "add", junk); !errors.Is(err, mirror.ErrNotEmpty) {
		t.Fatalf("mirror add on a non-empty path: %v\n%s", err, out)
	}
	a := filepath.Join(n.home, "archive")
	out := n.must("mirror", "add", a)
	if !strings.Contains(out, n.mirrorName()) || !strings.Contains(out, ": online") {
		t.Fatalf("mirror add: %s", out)
	}
	if mk, err := mirror.ReadMarker(sysfs.OS{}, a); err != nil || mk.ClusterID != hex.EncodeToString(n.cid[:]) {
		t.Fatalf("marker %+v %v", mk, err)
	}
	if saved, err := config.Load(n.path); err != nil || len(saved.Node.Mirrors) != 1 || saved.Node.Mirrors[0].Path != a {
		t.Fatalf("config mirrors %+v %v", saved.Node.Mirrors, err)
	}
	first := n.settled(1) // the schedule's first snapshot, begun by the add itself
	for _, r := range first {
		if !r.Auto || r.State != "complete" || r.Files != 1 || r.Mirror != n.mirrorName() {
			t.Fatalf("first snapshot %+v", r)
		}
	}
	// a retry keeps the row and its name, and adds no second config entry
	if out := n.must("mirror", "add", a); !strings.Contains(out, n.mirrorName()) {
		t.Fatalf("mirror add again: %s", out)
	}
	if saved, _ := config.Load(n.path); len(saved.Node.Mirrors) != 1 {
		t.Fatalf("a retry added a config entry: %+v", saved.Node.Mirrors)
	}
	n.must("snapshot", "create", "keep")
	before := n.settled(2)

	// the disk goes to another cluster: forgotten here, its marker rewritten there
	n.must("mirror", "remove", n.mirrorName())
	if rows := n.snapshots(); len(rows) != 0 {
		t.Fatalf("catalogue after mirror remove %+v", rows)
	}
	if _, err := mirror.Adopt(sysfs.OS{}, a, strings.Repeat("ff", 16), "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if out, err := n.run("mirror", "add", a); !errors.Is(err, mirror.ErrForeign) || !strings.Contains(err.Error(), "--adopt") {
		t.Fatalf("mirror add of a foreign mirror: %v\n%s", err, out)
	}
	if mk, _ := mirror.ReadMarker(sysfs.OS{}, a); mk.ClusterID != strings.Repeat("ff", 16) {
		t.Fatal("a refused add changed the foreign marker")
	}
	if out := n.must("mirror", "add", a, "--adopt"); !strings.Contains(out, ": online") {
		t.Fatalf("mirror add --adopt: %s", out)
	}
	if mk, _ := mirror.ReadMarker(sysfs.OS{}, a); mk.ClusterID != hex.EncodeToString(n.cid[:]) {
		t.Fatalf("adopted marker names %s", mk.ClusterID)
	}
	after := n.settled(2)
	for name, r := range before {
		got, ok := after[name]
		if !ok || got.State != "complete" || got.At != r.At || got.Auto != r.Auto || got.Files != r.Files || got.ID <= r.ID {
			t.Errorf("%s after --adopt: %+v (was %+v)", name, got, r)
		}
	}
	if out := n.must("snapshot", "restore", n.mirrorName()+"/keep", "/doc", "--as", "doc.back"); !strings.Contains(out, "doc.back") {
		t.Fatalf("restore from an adopted snapshot: %s", out)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/files/doc.back", n.cfg.Listen.Web))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != mirrorDoc {
		t.Fatalf("restored doc.back: %q", b)
	}
}

// `mirror remove` forgets the mirror and its catalogue rows and leaves the disk as it
// is; the disk added back, at another mount point here, lists every snapshot again
// (docs/16 §5).
// Invariant: I-188, I-183
func TestCLI_MirrorRemove_LeavesDisk(t *testing.T) {
	n := startMirrorNode(t)
	a := filepath.Join(n.home, "archive")
	n.must("mirror", "add", a)
	n.settled(1)
	n.must("snapshot", "create", "keep")
	before := n.settled(2)
	m, err := mirror.Open(sysfs.OS{}, a, "")
	if err != nil {
		t.Fatal(err)
	}
	used := m.Used()
	n.must("mirror", "remove", n.mirrorName())
	if out := n.must("mirror", "ls"); strings.Contains(out, n.mirrorName()) {
		t.Fatalf("mirror ls after remove: %s", out)
	}
	m, err = mirror.Open(sysfs.OS{}, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if mans, _ := m.Manifests(); len(mans) != 2 || m.Used() != used {
		t.Fatalf("the disk after mirror remove: %d manifests, %d bytes (was %d)", len(mans), m.Used(), used)
	}
	// the disk is plugged in at another mount point
	b := filepath.Join(n.home, "bigger")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	if out := n.must("mirror", "add", b); !strings.Contains(out, n.mirrorName()) {
		t.Fatalf("added back under another name: %s", out)
	}
	after := n.settled(2)
	for name, r := range before {
		if got, ok := after[name]; !ok || got.At != r.At || got.Files != r.Files || got.Mirror != n.mirrorName() {
			t.Errorf("%s added back: %+v (was %+v)", name, got, r)
		}
	}
}

// `mirror ls|cat|export <path>` read a mirror with no cluster: the node is stopped
// (docs/16 §2, §7).
// Invariant: I-180
func TestCLI_MirrorStandalone_LsCatExport(t *testing.T) {
	n := startMirrorNode(t)
	a := filepath.Join(n.home, "archive")
	n.must("mirror", "add", a)
	n.settled(1) // one sync per mirror: the schedule's first one ends before a manual one begins
	n.must("snapshot", "create", "keep")
	n.settled(2)
	n.stop()
	out := n.must("mirror", "ls", a)
	if !strings.Contains(out, "keep") || !strings.Contains(out, hex.EncodeToString(n.cid[:])) {
		t.Fatalf("mirror ls: %s", out)
	}
	if out := n.must("mirror", "cat", a, "keep", "/doc"); out != mirrorDoc {
		t.Fatalf("mirror cat: %q", out)
	}
	dst := filepath.Join(n.home, "rescue")
	n.must("mirror", "export", a, "keep", dst)
	if b, err := os.ReadFile(filepath.Join(dst, "doc")); err != nil || string(b) != mirrorDoc {
		t.Fatalf("exported doc: %q %v", b, err)
	}
	if _, err := n.run("mirror", "cat", a, "no-such-snapshot", "/doc"); !errors.Is(err, mirror.ErrNotFound) {
		t.Fatalf("cat of a missing snapshot: %v", err)
	}
	if mans, _ := func() ([]mirror.Manifest, error) {
		m, err := mirror.Open(sysfs.OS{}, a, "")
		if err != nil {
			return nil, err
		}
		return m.Manifests()
	}(); len(mans) != 2 {
		t.Fatalf("the standalone readers changed the disk: %d manifests", len(mans))
	}
}

// `tailraid export <dir>` copies a fresh Raft snapshot of the metadata out (docs/12 §9).
// Invariant: I-188
func TestCLI_Export(t *testing.T) {
	n := startMirrorNode(t)
	dst := filepath.Join(n.home, "meta-export")
	out := n.must("export", dst)
	if !strings.Contains(out, "metadata snapshot at index") {
		t.Fatalf("export: %s", out)
	}
	ents, err := os.ReadDir(dst)
	if err != nil || len(ents) != 1 {
		t.Fatalf("export dir %v %v", ents, err)
	}
	for _, f := range []string{"state.bin", "meta.json"} {
		if fi, err := os.Stat(filepath.Join(dst, ents[0].Name(), f)); err != nil || fi.Size() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Node.DataDir, "export")); err == nil {
		if left, _ := os.ReadDir(filepath.Join(n.cfg.Node.DataDir, "export")); len(left) != 0 {
			t.Errorf("the node's copy was left behind: %v", left)
		}
	}
}
