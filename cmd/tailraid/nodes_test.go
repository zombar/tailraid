package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
)

// node ls names a host's own store "<host> (data dir)" once it has disks, lists its disks
// under it, and flags targets on one physical disk (docs/14 §8).
func TestPrintNodes_DataDirAndSharedDisk(t *testing.T) {
	nodes := []admin.NodeReport{
		{Idx: 1, Name: "atlas", Kind: "device", State: "active", Live: true, DataDir: true, SharedWith: []string{"atlas/2"}},
		{Idx: 2, Name: "nomad", Kind: "device", State: "active", Live: true},
		{Idx: 3, Name: "atlas/1", Kind: "disk", Host: 1, State: "active", Live: true},
		{Idx: 4, Name: "atlas/2", Kind: "disk", Host: 1, State: "active", Live: true, SharedWith: []string{"atlas"}},
	}
	var b bytes.Buffer
	printNodes(&b, nodes)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("output:\n%s", b.String())
	}
	for i, want := range []string{"atlas (data dir)", "  atlas/1", "  atlas/2", "nomad"} {
		if !strings.Contains(lines[i+1], want) {
			t.Errorf("row %d %q lacks %q", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(lines[1], "shares disk with atlas/2") || !strings.Contains(lines[3], "shares disk with atlas") || strings.Contains(lines[2], "shares") {
		t.Errorf("flags:\n%s", b.String())
	}
	if strings.Contains(lines[4], "(data dir)") {
		t.Errorf("a host without disks is labelled: %q", lines[4])
	}
}
