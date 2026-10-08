package config

import (
	"os"
	"path/filepath"
	"testing"
)

// node.capacity is derived from the data disk unless pinned (docs/01 §Node budget).
// Invariant: I-74
func TestConfig_CapacityAuto(t *testing.T) {
	c := Default()
	c.Node.DataDir = "/tmp/c"
	if c.Node.Capacity != "" {
		t.Fatalf("default capacity %q, want derived", c.Node.Capacity)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("derived: %v", err)
	}
	c.Node.Capacity = "500G"
	if err := c.Validate(); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	c.Node.Capacity = "lots"
	if err := c.Validate(); err == nil {
		t.Fatal("bad capacity accepted")
	}
	// a config written by an older release keeps its pin; a file without the key derives
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("node:\n  data_dir: /tmp/c\n  capacity: 100G\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(p); err != nil || got.Node.Capacity != "100G" {
		t.Fatalf("old config: %+v %v", got.Node, err)
	}
	if err := os.WriteFile(p, []byte("node:\n  data_dir: /tmp/c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(p); err != nil || got.Node.Capacity != "" {
		t.Fatalf("new config: %+v %v", got.Node, err)
	}
}
