package admin_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/testharness"
)

// status warns when the data disk is under 20 % free, not at 20 % or above, and says
// nothing for an unknown disk.
//
// Invariant: I-162
func TestLowDiskWarning_Boundary(t *testing.T) {
	for _, c := range []struct {
		free, total uint64
		warn        bool
	}{{0, 100, true}, {1, 100, true}, {19, 100, true}, {20, 100, false}, {21, 100, false}, {100, 100, false}, {0, 0, false}, {5, 0, false}} {
		got := admin.LowDiskWarning(&admin.MachineReport{DiskFree: c.free, DiskTotal: c.total})
		if (got != "") != c.warn {
			t.Errorf("free %d of %d: %q, want warning %v", c.free, c.total, got, c.warn)
		}
	}
	if admin.LowDiskWarning(nil) != "" {
		t.Error("a nil machine report warns")
	}
}

// The report carries the trash (inodes and bytes, out of logical_bytes) from the FSM's
// stats and the node's cache block, which encodes with its pins as an array.
//
// Invariant: I-162
func TestReport_TrashAndCacheFields(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize)+5, 1)
	c.WriteFile(0, "/keep", data)
	c.WriteFile(0, "/gone", data[:100])
	c.Settle()
	if err := c.Nodes[0].FS.Remove("/gone"); err != nil {
		t.Fatal(err)
	}
	c.Settle()
	leader := c.Leader()
	rep := admin.Build(leader.Node, leader.Prober, c.CID, c.Clk, nil)
	if rep.Volume.TrashInodes != 1 || rep.Volume.TrashBytes != 100 || rep.Volume.LogicalBytes != uint64(len(data)) {
		t.Fatalf("volume %+v", rep.Volume)
	}
	rep.Cache = &pins.Report{Size: 1 << 30, Used: 4096, PinnedBytes: 2048, Pins: []pins.Entry{{Path: "/music", Bytes: 4096, CachedBytes: 2048}}}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"trash_inodes":1`, `"trash_bytes":100`, `"cache":{"size":1073741824,"used":4096,"pinned_bytes":2048,"pins":[{"path":"/music","bytes":4096,"cached_bytes":2048}]}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report JSON lacks %s", want)
		}
	}
	rep.Cache = &pins.Report{Pins: []pins.Entry{}}
	if b, _ = json.Marshal(rep); !strings.Contains(string(b), `"pins":[]`) {
		t.Errorf("an empty pin list is not an array: %s", b)
	}
}
