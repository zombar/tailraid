package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
)

// node ls lists a host's disks under it, flags a host with no disks and the disks on one
// physical disk, and prints no free space for a device, which stores nothing (docs/14 §8,
// §9).
// Invariant: I-118
func TestPrintNodes_DisklessAndSharedDisk(t *testing.T) {
	nodes := []admin.NodeReport{
		{Idx: 1, Name: "atlas", Kind: "device", State: "active", Live: true},
		{Idx: 2, Name: "nomad", Kind: "device", State: "active", Live: true, Diskless: true},
		{Idx: 3, Name: "atlas/1", Kind: "disk", Host: 1, State: "active", Live: true, Free: 5 << 30, SharedWith: []string{"atlas/2"}},
		{Idx: 4, Name: "atlas/2", Kind: "disk", Host: 1, State: "active", Live: true, Free: 5 << 30, SharedWith: []string{"atlas/1"}},
	}
	var b bytes.Buffer
	printNodes(&b, nodes)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("output:\n%s", b.String())
	}
	for i, want := range []string{"atlas", "  atlas/1", "  atlas/2", "nomad"} {
		if !strings.Contains(lines[i+1], want) {
			t.Errorf("row %d %q lacks %q", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(lines[2], "shares disk with atlas/2") || !strings.Contains(lines[3], "shares disk with atlas/1") || strings.Contains(lines[1], "shares") {
		t.Errorf("flags:\n%s", b.String())
	}
	if !strings.Contains(lines[4], "no disks") || strings.Contains(lines[1], "no disks") {
		t.Errorf("the diskless host is not flagged, or the host with disks is:\n%s", b.String())
	}
	if !strings.Contains(lines[1], "\t-\t") && !strings.Contains(lines[1], " - ") {
		t.Errorf("a device row shows free space: %q", lines[1])
	}
	if strings.Contains(lines[2], " - ") {
		t.Errorf("a disk row shows no free space: %q", lines[2])
	}
}
