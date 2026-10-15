package node

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
)

var testCluster = [16]byte{9, 9, 9}

// Invariant: I-86
func TestDiskMarker_Table(t *testing.T) {
	const host = meta.NodeIdx(1)
	mk := func(t *testing.T, cluster [16]byte, hostID string) (string, string) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "disk")
		id, err := PrepareDisk(p, cluster, hostID)
		if err != nil {
			t.Fatal(err)
		}
		return p, id
	}
	rows := func(id string, h meta.NodeIdx, st meta.NodeState) []meta.Node {
		return []meta.Node{{Idx: host, ID: "h1", Kind: meta.KindDevice, State: meta.Active}, {Idx: 7, ID: id, Kind: meta.KindDisk, Host: h, State: st}}
	}
	t.Run("ok", func(t *testing.T) {
		p, id := mk(t, testCluster, "h1")
		row, reason := checkDisk(p, testCluster, "h1", host, rows(id, host, meta.Active))
		if reason != "" || row.Idx != 7 {
			t.Fatalf("%q %+v", reason, row)
		}
	})
	t.Run("draining disk still opens", func(t *testing.T) {
		p, id := mk(t, testCluster, "h1")
		if _, reason := checkDisk(p, testCluster, "h1", host, rows(id, host, meta.Draining)); reason != "" {
			t.Fatal(reason)
		}
	})
	cases := []struct {
		name string
		prep func(t *testing.T) (string, []meta.Node)
	}{
		{"missing marker (unmounted path)", func(t *testing.T) (string, []meta.Node) {
			return t.TempDir(), nil
		}},
		{"garbled marker", func(t *testing.T) (string, []meta.Node) {
			p := t.TempDir()
			_ = os.WriteFile(filepath.Join(p, MarkerName), []byte("{"), 0o600)
			return p, nil
		}},
		{"another cluster", func(t *testing.T) (string, []meta.Node) {
			p, id := mk(t, [16]byte{1}, "h1")
			return p, rows(id, host, meta.Active)
		}},
		{"another host's marker", func(t *testing.T) (string, []meta.Node) {
			p, id := mk(t, testCluster, "h2")
			return p, rows(id, host, meta.Active)
		}},
		{"registered under another host", func(t *testing.T) (string, []meta.Node) {
			p, id := mk(t, testCluster, "h1")
			return p, rows(id, 5, meta.Active)
		}},
		{"not registered", func(t *testing.T) (string, []meta.Node) {
			p, _ := mk(t, testCluster, "h1")
			return p, rows("other", host, meta.Active)
		}},
		{"removed", func(t *testing.T) (string, []meta.Node) {
			p, id := mk(t, testCluster, "h1")
			return p, rows(id, host, meta.Removed)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, nodes := c.prep(t)
			if _, reason := checkDisk(p, testCluster, "h1", host, nodes); reason == "" {
				t.Fatal("opened")
			}
		})
	}
	// add-disk refuses a path holding anything but lost+found; a marker of this cluster and
	// host is reused (a retried init or join, I-121), one of another cluster refused
	p := t.TempDir()
	_ = os.Mkdir(filepath.Join(p, "lost+found"), 0o700)
	id, err := PrepareDisk(p, testCluster, "h1")
	if err != nil {
		t.Fatalf("lost+found refused: %v", err)
	}
	if again, err := PrepareDisk(p, testCluster, "h1"); err != nil || again != id {
		t.Fatalf("second prepare: %q %v, want the same id %q", again, err, id)
	}
	if _, err := PrepareDisk(p, [16]byte{9}, "h1"); !errors.Is(err, ErrDiskNotEmpty) {
		t.Fatalf("another cluster's marker: %v", err)
	}
	if _, err := PrepareDisk(p, testCluster, "h2"); !errors.Is(err, ErrDiskNotEmpty) {
		t.Fatalf("another host's marker: %v", err)
	}
}

// Invariant: I-86
// Lifecycle: L-195
func TestNode_ClosedDisk_NotServedNotSwept(t *testing.T) {
	good := filepath.Join(t.TempDir(), "good")
	goodID, err := PrepareDisk(good, testCluster, "h1")
	if err != nil {
		t.Fatal(err)
	}
	unmounted := t.TempDir() // an empty mount point: no marker
	var cfg config.Config
	cfg.Node.Disks = []config.Disk{{Path: good, Capacity: "1G"}, {Path: unmounted, Capacity: "1G"}}
	nodes := []meta.Node{{Idx: 1, ID: "h1", Kind: meta.KindDevice, State: meta.Active}, {Idx: 2, ID: goodID, Kind: meta.KindDisk, Host: 1, State: meta.Active}}
	d := newDiskSet(cfg, testCluster, "h1", 1, slog.New(slog.DiscardHandler))
	d.refresh(nodes)
	if open := d.open(); len(open) != 1 || open[0].idx != 2 {
		t.Fatalf("open %v", open)
	}
	if _, idx, ok := d.lookup(goodID); !ok || idx != 2 {
		t.Fatal("open disk not served")
	}
	if s := d.local("anything-else"); s != nil {
		t.Fatalf("closed disk has a store: %v", s)
	}
	h := d.health()
	if len(h) != 2 || !h[0].OK || h[0].ID != goodID || h[1].OK || h[1].Reason == "" {
		t.Fatalf("health %+v", h)
	}
	// nothing was written to the unmounted path
	if ents, _ := os.ReadDir(unmounted); len(ents) != 0 {
		t.Fatalf("closed disk path written: %v", ents)
	}
	// the disk is mounted later (its marker appears): the next refresh opens it
	lateID, err := PrepareDisk(filepath.Join(unmounted, "x"), testCluster, "h1")
	if err != nil {
		t.Fatal(err)
	}
	d.disks[1].cfg.Path = filepath.Join(unmounted, "x")
	nodes = append(nodes, meta.Node{Idx: 3, ID: lateID, Kind: meta.KindDisk, Host: 1, State: meta.Active})
	d.refresh(nodes)
	if len(d.open()) != 2 {
		t.Fatal("late disk not opened")
	}
}

// Invariant: I-91
func TestNode_Rebudget_PerTarget(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *chunk.Store {
		s, err := chunk.Open(sysfs.OS{}, filepath.Join(dir, name), 0)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b, pinned := open("a"), open("b"), open("p")
	pinned.SetCapacity(123)
	got, err := budgets([]target{{idx: 1, store: a}, {idx: 2, store: b}, {idx: 3, store: pinned, pinned: true}})
	if err != nil {
		t.Fatal(err)
	}
	used, free, total, _ := a.Disk()
	whole := chunk.Budget(used, free, total)
	if _, ok := got[3]; ok || pinned.Capacity() != 123 {
		t.Fatal("a pinned target was re-derived")
	}
	// a and b share the temp filesystem: each gets half, never the whole. The filesystem
	// is the real one and other tests write to it, so the second reading may differ from
	// the one budgets() used by a few blocks: allow that drift, not a whole budget.
	const drift = 64 << 20
	if got[1] != got[2] || got[1]+got[2] > whole+drift || (whole > 1 && got[1] == whole) {
		t.Fatalf("shared budgets %d, %d of %d", got[1], got[2], whole)
	}
	if a.Capacity() != int64(got[1]) {
		t.Fatal("store cap does not follow its budget")
	}
}

// A disk this host cannot write — a read-only mount, or a path the service sandbox does
// not let it write (a disk added after `install` wrote the systemd unit) — is closed
// with the reason and reported not OK, so nothing is placed on it; it reopens once it is
// writable again. Checked when it opens and on every refresh (docs/14 §3).
// Invariant: I-146
func TestDiskSet_UnwritableDiskClosed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot write: not on Windows, not as root")
	}
	p := filepath.Join(t.TempDir(), "disk")
	id, err := PrepareDisk(p, testCluster, "h1")
	if err != nil {
		t.Fatal(err)
	}
	rows := []meta.Node{{Idx: 1, ID: "h1", Kind: meta.KindDevice, State: meta.Active}, {Idx: 7, ID: id, Kind: meta.KindDisk, Host: 1, State: meta.Active}}
	d := newDiskSet(config.Config{}, testCluster, "h1", 1, slog.Default())
	d.add(config.Disk{Path: p, Capacity: "1G"})
	health := func() rpc.DiskHealth {
		t.Helper()
		hs := d.health()
		if len(hs) != 1 {
			t.Fatalf("health %+v", hs)
		}
		return hs[0]
	}
	d.refresh(rows)
	if h := health(); !h.OK {
		t.Fatalf("writable disk not OK: %+v", h)
	}
	shards := filepath.Join(p, "shards")
	if err := os.Chmod(shards, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shards, 0o700) })
	d.refresh(rows) // an open disk that stopped being writable
	if h := health(); h.OK || !strings.Contains(h.Reason, "not writable") {
		t.Fatalf("read-only open disk: %+v", h)
	}
	if _, _, ok := d.lookup(id); ok {
		t.Fatal("a read-only disk still serves shards")
	}
	d2 := newDiskSet(config.Config{}, testCluster, "h1", 1, slog.Default()) // and one that never was
	d2.add(config.Disk{Path: p, Capacity: "1G"})
	d2.refresh(rows)
	if hs := d2.health(); len(hs) != 1 || hs[0].OK {
		t.Fatalf("read-only disk opened: %+v", hs)
	}
	if err := os.Chmod(shards, 0o700); err != nil {
		t.Fatal(err)
	}
	d.refresh(rows)
	if h := health(); !h.OK {
		t.Fatalf("writable again but still closed: %+v", h)
	}
}

// A configured disk whose marker is this host's and this cluster's but which has no row
// is the one case serve registers itself (docs/14 §2): a join cut before its
// registration; every other closed disk stays closed with its reason.
func TestDiskSet_Unregistered(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk")
	id, err := PrepareDisk(p, testCluster, "h1")
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if _, err := PrepareDisk(other, testCluster, "h2"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Node.Disks = []config.Disk{{Path: p}, {Path: other}, {Path: filepath.Join(t.TempDir(), "unmounted")}}
	d := newDiskSet(cfg, testCluster, "h1", 1, slog.New(slog.DiscardHandler))
	host := meta.Node{Idx: 1, ID: "h1", Kind: meta.KindDevice, State: meta.Active}
	d.refresh([]meta.Node{host})
	if got := d.unregistered(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("unregistered %v, want [0]: another host's disk and an unmounted path are not ours to register", got)
	}
	d.refresh([]meta.Node{host, {Idx: 7, ID: id, Kind: meta.KindDisk, Host: 1, State: meta.Active}})
	if got := d.unregistered(); len(got) != 0 {
		t.Fatalf("unregistered %v after its row arrived", got)
	}
}

// A disk removed from the cluster and added back on the same path keeps its marker, so
// two rows carry its ID: the removed one and the new active one. The host opens it under
// the active row; only a disk whose every row is removed stays closed as removed
// (docs/14 §2). Found by soak phase 13 on a rig that kept a previous run's rows: the
// re-added disk never opened and stayed down for the rest of the run.
// Invariant: I-90
func TestDiskSet_ReaddedAfterRemoval(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk")
	id, err := PrepareDisk(p, testCluster, "h1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Node.Disks = []config.Disk{{Path: p}}
	d := newDiskSet(cfg, testCluster, "h1", 1, slog.New(slog.DiscardHandler))
	host := meta.Node{Idx: 1, ID: "h1", Kind: meta.KindDevice, State: meta.Active}
	removed := meta.Node{Idx: 14, ID: id, Name: "h1/3", Kind: meta.KindDisk, Host: 1, State: meta.Removed}
	d.refresh([]meta.Node{host, removed})
	if st := d.state(p); st != "disk was removed from the cluster" {
		t.Fatalf("with its only row removed: %q", st)
	}
	again := meta.Node{Idx: 16, ID: id, Name: "h1/3", Kind: meta.KindDisk, Host: 1, State: meta.Active}
	d.refresh([]meta.Node{host, removed, again})
	if st := d.state(p); st != "open" {
		t.Fatalf("added back after a removal: %q, want open", st)
	}
	if open := d.open(); len(open) != 1 || open[0].idx != 16 {
		t.Fatalf("the re-added disk opened as %+v, want row 16", open)
	}
}
