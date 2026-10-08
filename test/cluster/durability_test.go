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

// writeAt writes data at off through gateway i (no flush).
func writeAt(t *testing.T, c *testharness.Cluster, i int, path string, off int, data []byte) {
	t.Helper()
	fh, err := c.Nodes[i].FS.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	if _, err := fh.Seek(int64(off), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
}

func flushAll(t *testing.T, c *testharness.Cluster, i int) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		if err = c.Nodes[i].Flush.FlushAll(ctx); err == nil {
			c.Settle()
			return
		}
		<-time.After(100 * time.Millisecond)
	}
	t.Fatalf("n%d flush: %v", i, err)
}

// A gateway whose overlay entry predates another gateway's Commit must not cut or shorten
// the bytes that Commit added: a write keeps the rewritten chunk full-length, and a
// truncate cuts at the requested point only (the model checker found a stale entry
// turning a truncate into one at the entry's old base size, zeroing a whole chunk).
// Invariant: I-12, I-35
// Lifecycle: L-178
func TestCluster_StaleGatewayOverlay_KeepsOtherGatewaysBytes(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	model := make([]byte, 0)
	apply := func(off int, data []byte) {
		if end := off + len(data); end > len(model) {
			model = append(model, make([]byte, end-len(model))...)
		}
		copy(model[off:], data)
	}
	check := func(what string) {
		t.Helper()
		for i := range c.Nodes {
			got, err := c.ReadFileCold(i, "/f")
			if err != nil || !bytes.Equal(got, model) {
				t.Fatalf("%s: n%d: err=%v len %d want %d, differs at %d", what, i, err, len(got), len(model), testharness.FirstDiff(got, model))
			}
		}
	}
	// n0 creates a short file and flushes: its overlay entry remembers base size 100
	d0 := testharness.Pattern(100, 1)
	writeAt(t, c, 0, "/f", 0, d0)
	apply(0, d0)
	flushAll(t, c, 0)
	// n1 grows it to two and a bit chunks, filling the rest of chunk 0 on the way
	d1 := testharness.Pattern(2*cs+10-100, 2)
	writeAt(t, c, 1, "/f", 100, d1)
	apply(100, d1)
	flushAll(t, c, 1)
	check("after n1 grew the file")
	// n0 writes 50 bytes at 0: chunk 0 must stay full-length, the rest untouched
	d2 := testharness.Pattern(50, 3)
	writeAt(t, c, 0, "/f", 0, d2)
	apply(0, d2)
	flushAll(t, c, 0)
	check("after n0 rewrote the head through its stale entry")
	// the same with n0's entry still dirty while n1 commits: on /g (100 bytes) n0 writes
	// 40 bytes without flushing, n1 fills chunk 0 and starts chunk 1, flushes first, and
	// n0's flush rebases onto the new base: its rewritten chunk 0 must be full-length
	g := make([]byte, 0)
	applyG := func(off int, data []byte) {
		if end := off + len(data); end > len(g) {
			g = append(g, make([]byte, end-len(g))...)
		}
		copy(g[off:], data)
	}
	g0 := testharness.Pattern(100, 5)
	writeAt(t, c, 0, "/g", 0, g0)
	applyG(0, g0)
	flushAll(t, c, 0)
	g1 := testharness.Pattern(40, 6)
	writeAt(t, c, 0, "/g", 0, g1)
	applyG(0, g1)
	g2 := testharness.Pattern(cs-100+10, 7)
	writeAt(t, c, 1, "/g", 100, g2)
	applyG(100, g2)
	flushAll(t, c, 1)
	flushAll(t, c, 0)
	for i := range c.Nodes {
		got, err := c.ReadFileCold(i, "/g")
		if err != nil || !bytes.Equal(got, g) {
			t.Fatalf("after n0 flushed a dirty entry older than n1's commit: n%d: err=%v len %d want %d, differs at %d", i, err, len(got), len(g), testharness.FirstDiff(got, g))
		}
	}
	// n1 extends further, then n0 truncates inside chunk 2: only bytes past the point go
	d3 := testharness.Pattern(cs, 4)
	writeAt(t, c, 1, "/f", 2*cs+10, d3)
	apply(2*cs+10, d3)
	flushAll(t, c, 1)
	cut := 2*cs + 100
	fh, err := c.Nodes[0].FS.OpenFile("/f", os.O_WRONLY, 0)
	must(t, err)
	must(t, fh.Truncate(int64(cut)))
	_ = fh.Close()
	model = model[:cut]
	flushAll(t, c, 0)
	check("after n0 truncated through its stale entry")
	// and GETATTR on n0 reported the committed size all along (no truncate pending)
	st, err := c.Nodes[0].FS.Stat("/f")
	must(t, err)
	if st.Size() != int64(cut) {
		t.Fatalf("stat size %d want %d", st.Size(), cut)
	}
}
