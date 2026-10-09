package admin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/testharness"
)

// status labels a host's own store when it has added disks, and marks the targets of one
// host that sit on one filesystem: they fail together (docs/14 §8). The harness keeps
// every store in one temp filesystem, so a host's store and its disks all share.
func TestReport_DataDirAndSharedDisk(t *testing.T) {
	c := testharness.NewCluster(t, 2, 2, 1)
	defer c.Close()
	c.AddDisk(0)
	c.AddDisk(0)
	c.Probe()
	nd := c.Nodes[0]
	rep := admin.Build(nd.Node, nd.Prober, c.CID, c.Clk, nil)
	byName := map[string]admin.NodeReport{}
	for _, n := range rep.Nodes {
		byName[n.Name] = n
	}
	host, d1, d2, other := byName["n0"], byName["n0/1"], byName["n0/2"], byName["n1"]
	if !host.DataDir || other.DataDir || d1.DataDir {
		t.Fatalf("data_dir: host %v, other host %v, disk %v", host.DataDir, other.DataDir, d1.DataDir)
	}
	if !slices.Equal(host.SharedWith, []string{"n0/1", "n0/2"}) || !slices.Equal(d1.SharedWith, []string{"n0", "n0/2"}) || !slices.Equal(d2.SharedWith, []string{"n0", "n0/1"}) {
		t.Fatalf("shared_with: %v %v %v", host.SharedWith, d1.SharedWith, d2.SharedWith)
	}
	if len(other.SharedWith) != 0 {
		t.Fatalf("a host without disks shares with %v", other.SharedWith)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "n0 (data dir), n0/1 and n0/2 are on one physical disk") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no shared-disk warning in %q", rep.Warnings)
	}
}
