package admin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/testharness"
)

// status flags a host with no disks as diskless (docs/14 §9) and marks the disks of one
// host that sit on one filesystem: they fail together (docs/14 §8). The harness keeps
// every store in one temp filesystem, so a host's disks all share.
// Invariant: I-118
func TestReport_DisklessAndSharedDisk(t *testing.T) {
	c := testharness.NewCluster(t, 2, 2, 1) // n0/1 and n1/1 by default
	defer c.Close()
	c.AddDisk(0) // n0/2
	c.AddDisklessNode(false)
	c.Probe()
	nd := c.Nodes[0]
	rep := admin.Build(nd.Node, nd.Prober, c.CID, c.Clk, nil)
	byName := map[string]admin.NodeReport{}
	for _, n := range rep.Nodes {
		byName[n.Name] = n
	}
	host, d1, d2, other, bare := byName["n0"], byName["n0/1"], byName["n0/2"], byName["n1"], byName["n2"]
	if host.Diskless || other.Diskless || !bare.Diskless || d1.Diskless {
		t.Fatalf("diskless: n0 %v, n1 %v, n2 %v, n0/1 %v", host.Diskless, other.Diskless, bare.Diskless, d1.Diskless)
	}
	if host.Capacity != 0 || bare.Capacity != 0 || d1.Capacity == 0 {
		t.Fatalf("capacity: device rows %d %d, disk %d", host.Capacity, bare.Capacity, d1.Capacity)
	}
	if !slices.Equal(d1.SharedWith, []string{"n0/2"}) || !slices.Equal(d2.SharedWith, []string{"n0/1"}) || len(host.SharedWith) != 0 {
		t.Fatalf("shared_with: host %v, disks %v %v", host.SharedWith, d1.SharedWith, d2.SharedWith)
	}
	if len(byName["n1/1"].SharedWith) != 0 {
		t.Fatalf("a host's only disk shares with %v", byName["n1/1"].SharedWith)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "n0/1 and n0/2 are on one physical disk") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no shared-disk warning in %q", rep.Warnings)
	}
}
