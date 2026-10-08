package cluster_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// crashPoint names a flusher hook where the gateway is killed mid-flush (docs/05 §2,
// C3–C7: C1/C2 are inside the WAL append and are covered by the WAL torn-tail tests).
type crashPoint struct {
	name string
	set  func(h *vfs.Hooks, fn func())
}

var crashPoints = []crashPoint{
	{"C6 after all shards acked, before Commit", func(h *vfs.Hooks, fn func()) { h.AfterAllShardsAcked = fn }},
	{"C6b before Commit", func(h *vfs.Hooks, fn func()) { h.BeforeCommit = fn }},
	{"C7 after Raft commit, before WAL reset", func(h *vfs.Hooks, fn func()) { h.AfterRaftCommit = fn }},
}

// The two-outcome rule (I-38): after a crash at any point, the file is either exactly
// the old content with no visible orphans, or exactly the new content on every gateway.
// Invariant: I-7, I-12, I-38
// Lifecycle: L-115, L-116, L-118, L-119, L-121
func TestCrash_AtEveryPoint(t *testing.T) {
	for _, cp := range crashPoints {
		t.Run(cp.name, func(t *testing.T) {
			c := testharness.NewCluster(t, 3, 2, 1)
			defer c.Close()
			old := testharness.Pattern(int(testharness.ChunkSize)+10, 1)
			c.WriteFile(0, "/f", old)
			c.Settle()
			gw := c.Nodes[0]
			newData := testharness.Pattern(int(testharness.ChunkSize)+10, 2)
			fh, err := gw.FS.OpenFile("/f", os.O_RDWR, 0)
			must(t, err)
			_, _ = fh.Write(newData)
			_ = fh.Close()
			// the WRITE was acknowledged (fsynced WAL); now crash the gateway at the hook
			killed := make(chan struct{})
			cp.set(&gw.Flush.Hooks, func() {
				go c.Kill(0) // from another goroutine: Kill closes what the flusher is using
				<-killed
			})
			go func() {
				testharness.Eventually(t, func() bool { return !c.Alive(0) }, "kill")
				close(killed)
			}()
			_ = gw.Flush.FlushAll(ctx) // fails or succeeds; either way the gateway is dead now
			testharness.Eventually(t, func() bool { return !c.Alive(0) }, "gateway dead")
			// the surviving gateways see exactly old or exactly new, never a mixture
			c.Settle()
			seen := map[string]bool{}
			for i := 1; i < 3; i++ {
				got, err := c.ReadFileCold(i, "/f")
				must(t, err)
				switch {
				case bytes.Equal(got, old):
					seen["old"] = true
				case bytes.Equal(got, newData):
					seen["new"] = true
				default:
					t.Fatalf("n%d sees a third version (len %d, first diff vs new %d)", i, len(got), testharness.FirstDiff(got, newData))
				}
			}
			if len(seen) != 1 {
				t.Fatalf("gateways disagree: %v", seen)
			}
			// the crashed gateway restarts: its WAL replays and the acknowledged write lands
			c.Restart(0)
			c.Settle()
			must(t, c.Nodes[0].Flush.FlushAll(ctx))
			c.Settle()
			for i := range c.Nodes {
				got, err := c.ReadFileCold(i, "/f")
				if err != nil || !bytes.Equal(got, newData) {
					t.Fatalf("after restart n%d: %v (acknowledged write lost)", i, err)
				}
			}
		})
	}
}

// Every acknowledged, flushed write is readable from any k survivors after the gateway
// and m other nodes die.
// Invariant: I-12
// Lifecycle: L-74, L-120
func TestDurability_AckImpliesReadableFromKSurvivors(t *testing.T) {
	iters := 6
	if !testing.Short() {
		iters = 40
	}
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	for i := 0; i < iters; i++ {
		gw := i % 4
		if !c.Alive(gw) {
			c.Restart(gw)
			c.Settle()
		}
		data := testharness.Pattern(int(testharness.ChunkSize)+i, byte(i))
		path := fmt.Sprintf("/f%02d", i)
		c.WriteFile(gw, path, data) // ack + flush
		c.Settle()
		// kill the gateway (m = 1 loss, and it may hold a slot); every survivor reads it cold
		c.Kill(gw)
		c.Clk.Advance(cluster.LiveThreshold + time.Second)
		c.Probe()
		for reader := range c.Nodes {
			if reader == gw {
				continue
			}
			got, err := c.ReadFileCold(reader, path)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("iter %d: read from survivor n%d after killing gateway n%d: %v", i, reader, gw, err)
			}
		}
		c.Restart(gw)
		c.Settle()
	}
	_ = context.Background
}
