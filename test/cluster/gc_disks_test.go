package cluster_test

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

// GC deletes a collected chunk's shards on every live target, disks included: a shard on
// an added disk does not wait for that disk's sweep, and Deleted counts it.
//
// Invariant: I-13, I-86
func TestGC_DeletesShardsOnDisks(t *testing.T) {
	c := testharness.NewCluster(t, 1, 2, 2)
	defer c.Close()
	disks := map[meta.NodeIdx]bool{}
	for range 4 {
		disks[c.AddDisk(0)] = true
	}
	c.Probe() // the prober learns the disks through the host's health row
	c.WriteFile(0, "/f", testharness.Pattern(int(testharness.ChunkSize), 21))
	c.Settle()
	ch := c.ChunksOf(0, "/f")[0]
	onDisks := map[meta.NodeIdx]chunk.ShardKey{}
	for i, s := range ch.Placement {
		if disks[s] {
			onDisks[s] = chunk.Key(ch.Hash, ch.K, ch.M, i)
		}
	}
	if len(onDisks) == 0 {
		t.Fatalf("no slot on an added disk: placement %v", ch.Placement)
	}
	must(t, c.Nodes[0].FS.Remove("/f"))
	c.Settle()
	purgeTrash(t, c) // a delete reclaims nothing until the entry is purged (docs/06 §1)
	leader := c.Leader()
	gc := repair.NewGC(leader.Node.FSM, leader.Node, leader.Client, leader.Prober, c.Clk, 10*time.Minute, nil)
	c.Clk.Advance(11 * time.Minute)
	c.Probe() // liveness is clocked: the disks are alive again at the new time
	for d, k := range onDisks {
		ageShardFile(t, c.DiskStore(0, d).Path(k), 11*time.Minute) // past the fence's age guard too
	}
	res, err := gc.RunOnce(ctx)
	must(t, err)
	if res.Victims != 1 || res.Deleted < len(onDisks) {
		t.Fatalf("gc %+v, want 1 victim and at least %d deleted (the disk slots)", res, len(onDisks))
	}
	for d, k := range onDisks {
		if c.DiskStore(0, d).Has(k) {
			t.Fatalf("shard %s still on disk %d after GC", k, d)
		}
	}
}
