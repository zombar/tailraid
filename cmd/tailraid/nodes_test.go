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
	printNodes(&b, nodes, false)
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

// A removed member keeps its row in the table for good (its index is never reused), so
// status and node ls leave it out: a device that was removed and joined again shows once,
// under its new index. node ls --all lists the removed rows too, and --json always
// carries them (docs/14 §8).
// Invariant: I-132
func TestPrintNodes_HidesRemoved(t *testing.T) {
	nodes := []admin.NodeReport{
		{Idx: 1, Name: "jockey", Kind: "device", State: "active", Live: true},
		{Idx: 2, Name: "jockey/1", Kind: "disk", Host: 1, State: "active", Live: true, Free: 20 << 30},
		{Idx: 3, Name: "honker", Kind: "device", State: "removed", Addr: "100.64.0.2"},
		{Idx: 4, Name: "honker", Kind: "device", State: "active", Addr: "100.64.0.2", Live: false, DeadFor: "3h", Diskless: true},
		{Idx: 5, Name: "honker/1", Kind: "disk", Host: 3, State: "removed"},
	}
	for _, c := range []struct {
		all  bool
		rows []string
	}{
		{false, []string{"jockey", "  jockey/1", "honker"}},
		{true, []string{"jockey", "  jockey/1", "honker", "  honker/1", "honker"}},
	} {
		var b bytes.Buffer
		printNodes(&b, nodes, c.all)
		lines := strings.Split(strings.TrimSpace(b.String()), "\n")
		if len(lines) != len(c.rows)+1 {
			t.Fatalf("all=%v: %d rows, want %d:\n%s", c.all, len(lines)-1, len(c.rows), b.String())
		}
		for i, want := range c.rows {
			if !strings.Contains(lines[i+1], want) {
				t.Errorf("all=%v row %d %q lacks %q", c.all, i+1, lines[i+1], want)
			}
		}
		if removed := strings.Count(b.String(), "removed"); removed != map[bool]int{false: 0, true: 2}[c.all] {
			t.Errorf("all=%v: %d removed rows shown:\n%s", c.all, removed, b.String())
		}
	}
	if len(nodes) != 5 || nodes[2].State != "removed" {
		t.Error("printNodes must not edit the report it was given")
	}
}

// A device reset and joined again leaves a removed row under its old name: a name names
// the row that is not removed, an index or ID names exactly that row, and a name only a
// removed row has still resolves (node describe of a removed device) (docs/02 §Lifecycle
// commands).
// Invariant: I-136
func TestFindNode_PrefersLiveRow(t *testing.T) {
	rep := admin.Report{Nodes: []admin.NodeReport{
		{Idx: 1, ID: "aaaa", Name: "jockey", State: "active"},
		{Idx: 3, ID: "bbbb", Name: "honker", State: "removed"},
		{Idx: 4, ID: "cccc", Name: "honker", State: "active"},
		{Idx: 5, ID: "dddd", Name: "old", State: "removed"},
	}}
	for _, c := range []struct {
		key  string
		want uint32
	}{{"honker", 4}, {"3", 3}, {"bbbb", 3}, {"4", 4}, {"old", 5}, {"jockey", 1}} {
		n, err := findNode(rep, c.key)
		if err != nil || uint32(n.Idx) != c.want {
			t.Errorf("findNode(%q) = %d %v, want %d", c.key, n.Idx, err, c.want)
		}
	}
	if _, err := findNode(rep, "nope"); err == nil {
		t.Error("an unknown name resolved")
	}
}
