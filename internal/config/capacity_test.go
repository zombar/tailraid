package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A disk's capacity is derived from its filesystem unless pinned; a device row has no
// budget, so a config that still pins node.capacity is refused (docs/01 §Node budget,
// docs/14 §9).
// Invariant: I-74
func TestConfig_DiskCapacity(t *testing.T) {
	c := Default()
	c.Node.DataDir = "/tmp/c"
	c.Node.Disks = []Disk{{Path: "/tmp/d1"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("derived: %v", err)
	}
	c.Node.Disks[0].Capacity = "500G"
	if err := c.Validate(); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	c.Node.Disks[0].Capacity = "lots"
	if err := c.Validate(); err == nil {
		t.Fatal("bad capacity accepted")
	}
	// a file without a capacity derives; one from before devices stopped being targets
	// (node.capacity) fails the strict load, which names the key
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("node:\n  data_dir: /tmp/c\n  disks:\n    - path: /tmp/d1\n    - path: /tmp/d2\n      capacity: 100G\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || len(got.Node.Disks) != 2 || got.Node.Disks[0].Capacity != "" || got.Node.Disks[1].Capacity != "100G" {
		t.Fatalf("disks: %+v %v", got.Node.Disks, err)
	}
	if err := os.WriteFile(p, []byte("node:\n  data_dir: /tmp/c\n  capacity: 100G\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("a config pinning node.capacity was accepted")
	}
}
