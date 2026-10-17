package admin_test

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// The report carries the process's runtime view and the gateway's flush and WAL bounds,
// which the soak sampler records (docs/12 §8).
// Invariant: I-84
func TestReport_ProcessAndFlushFields(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	nd := c.Nodes[0]
	rep := admin.Build(nd.Node, nd.Prober, c.CID, c.Clk, nil)
	if rep.Process == nil || rep.Process.HeapInuse == 0 || rep.Process.Sys < rep.Process.HeapInuse || rep.Process.Goroutines < 1 {
		t.Fatalf("process %+v", rep.Process)
	}
	infos := []vfs.DirtyInfo{{Ino: 5, DirtyBytes: 100, PlanBytes: 300, WALBytes: 500, Flushing: true}, {Ino: 6, DirtyBytes: 7, LastErr: "too few acks"}}
	g := admin.GatewayFromDirty(infos).WithFlusher(nd.Flush, nd.Ov)
	if g.DirtyInodes != 2 || g.DirtyBytes != 107 || g.PlanBytes != 300 || g.WALBytes != 500 || g.Flushing != 1 || g.LastFlushErr != "too few acks" {
		t.Fatalf("gateway %+v", g)
	}
	if g.WALCap != max(uint64(vfs.DefaultWALCap), g.Stage) || g.Stage < vfs.MinStage || g.FlushCap == 0 || g.FlushBytes != 0 {
		t.Fatalf("bounds %+v", g)
	}
	c.WriteFile(0, "/f", testharness.Pattern(2*int(testharness.ChunkSize), 1))
	if _, peak := nd.Flush.Buffers(); peak == 0 || peak > nd.Flush.BufferCap() {
		t.Fatalf("flush peak %d, cap %d", peak, nd.Flush.BufferCap())
	}
	_ = errors.New
}
