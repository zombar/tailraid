package node

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// Growing a disk in place with a pinned budget: raise the disk's capacity and restart.
// The store takes the new cap at once, and the maintenance tick registers it with the
// cluster so placement and status see it; a pinned value is exact, so no hysteresis
// applies.
// Invariant: I-74
func TestNode_Rebudget_PinnedChangeRegistered(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0)
	defer c.Close()
	nd := c.Nodes[0]
	ops := opsFor(c, nd)
	path := filepath.Join(t.TempDir(), "pinned")
	id, err := PrepareDisk(path, c.CID, nd.ID)
	must(t, err)
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "add_disk", NodeIdx: nd.Idx(), DiskID: id, Name: "n0/9", Capacity: 1 << 30}))
	c.Settle()
	ops.disks = newDiskSet(config.Config{}, c.CID, nd.ID, nd.Idx(), slog.Default())
	ops.disks.add(config.Disk{Path: path, Capacity: "1G"}) // pinned
	if got := ops.openDisk(ctx, path); got != "open" {
		t.Fatalf("open_disk = %q", got)
	}
	row, _ := nd.Node.FSM.NodeByID(id)
	target := ops.targets()[0]
	for _, want := range []uint64{row.Capacity + 1, row.Capacity * 2, 0} {
		target.store.SetCapacity(int64(want))
		ops.rebudget(ctx)
		c.Settle()
		if got, _ := nd.Node.FSM.Node(row.Idx); got.Capacity != want {
			t.Fatalf("pinned %d: the cluster holds %d", want, got.Capacity)
		}
	}
	// unchanged: nothing is proposed (a proposal is applied before MemberOp returns)
	applied := nd.Node.AppliedIndex()
	ops.rebudget(ctx)
	if nd.Node.AppliedIndex() != applied {
		t.Fatal("an unchanged pinned budget was proposed again")
	}
	// a device row never gets a budget: set_capacity on it is refused (docs/14 §9)
	if err := c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_capacity", NodeIdx: nd.Idx(), Capacity: 5}); err == nil {
		t.Fatal("set_capacity on a device accepted")
	}
}

// A restart after `volume grow` registers the new pinned budget as the maintenance loop
// starts, not a tick later: `status` reads the disk's free space from its host at once,
// so the node table must not lag it by a minute (the dashboard draws used = capacity −
// free from the two).
// Invariant: I-74
func TestNode_Maintenance_RegistersBudgetAtStart(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0)
	defer c.Close()
	nd := c.Nodes[0]
	ops := opsFor(c, nd)
	path := filepath.Join(t.TempDir(), "pinned")
	id, err := PrepareDisk(path, c.CID, nd.ID)
	must(t, err)
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "add_disk", NodeIdx: nd.Idx(), DiskID: id, Name: "n0/9", Capacity: 1 << 30}))
	c.Settle()
	ops.disks = newDiskSet(config.Config{}, c.CID, nd.ID, nd.Idx(), slog.Default())
	ops.disks.add(config.Disk{Path: path, Capacity: "2G"}) // the pin raised, then a restart
	if got := ops.openDisk(ctx, path); got != "open" {
		t.Fatalf("open_disk = %q", got)
	}
	row, _ := nd.Node.FSM.NodeByID(id)
	mctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { ops.maintenance(mctx, repair.DefaultConfig()); close(done) }()
	defer func() { cancel(); <-done }()
	// the fake clock never moves: only a pass at start can register it
	testharness.Eventually(t, func() bool {
		got, _ := nd.Node.FSM.Node(row.Idx)
		return got.Capacity == 2<<30
	}, "the raised pin registered at start")
}
