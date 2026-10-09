package node

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// add-disk asks the host to open the disk right after registering it through the leader;
// the host may not have applied that entry yet, so open_disk waits for the row instead of
// leaving the disk closed until the next maintenance tick (docs/14 §2).
// Invariant: I-90
func TestOpenDisk_WaitsForItsRow(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0)
	defer c.Close()
	nd := c.Nodes[0]
	ops := opsFor(c, nd)
	ops.disks = newDiskSet(config.Config{}, c.CID, nd.ID, nd.Idx(), slog.Default())
	open := func(path string) chan string {
		ops.disks.add(config.Disk{Path: path, Capacity: "1G"})
		done := make(chan string, 1)
		go func() { done <- ops.openDisk(ctx, path) }()
		return done
	}
	wait := func(done chan string) string {
		var got string
		testharness.Eventually(t, func() bool {
			c.Clk.Advance(100 * time.Millisecond)
			select {
			case got = <-done:
				return true
			default:
				return false
			}
		}, "open_disk returned")
		return got
	}

	path := filepath.Join(t.TempDir(), "d1")
	id, err := PrepareDisk(path, c.CID, nd.ID)
	must(t, err)
	done := open(path) // the row is not there yet
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "add_disk", NodeIdx: nd.Idx(), DiskID: id, Name: "n0/1", Capacity: 1 << 30}))
	if got := wait(done); got != "open" {
		t.Fatalf("open_disk = %q, want open once the row arrived", got)
	}
	// a disk that is never registered gives up after the wait with the reason
	orphan := filepath.Join(t.TempDir(), "d2")
	_, err = PrepareDisk(orphan, c.CID, nd.ID)
	must(t, err)
	if got := wait(open(orphan)); got == "open" || got == "" {
		t.Fatalf("unregistered disk: %q", got)
	}
}
