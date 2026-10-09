package node

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
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
	// add-disk refuses a path holding anything but lost+found, and an existing marker
	p := t.TempDir()
	_ = os.Mkdir(filepath.Join(p, "lost+found"), 0o700)
	if _, err := PrepareDisk(p, testCluster, "h1"); err != nil {
		t.Fatalf("lost+found refused: %v", err)
	}
	if _, err := PrepareDisk(p, testCluster, "h1"); !errors.Is(err, ErrDiskNotEmpty) {
		t.Fatalf("second prepare: %v", err)
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
