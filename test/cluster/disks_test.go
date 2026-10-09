package cluster_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// overwriteAt writes p at off into path through node i and flushes.
func overwriteAt(t *testing.T, c *testharness.Cluster, i int, path string, off int64, p []byte) {
	t.Helper()
	nd := c.Nodes[i]
	f, err := nd.FS.OpenFile(path, os.O_RDWR, 0)
	must(t, err)
	_, err = f.Seek(off, io.SeekStart)
	must(t, err)
	_, err = f.Write(p)
	must(t, err)
	must(t, f.Close())
	must(t, nd.Flush.FlushAll(context.Background()))
}

// hostOf maps a target to its host in node i's table.
func hostOf(c *testharness.Cluster, i int, n meta.NodeIdx) meta.NodeIdx {
	nd, _ := c.Nodes[i].Node.FSM.Node(n)
	if nd.Kind == meta.KindDisk {
		return nd.Host
	}
	return n
}

// perHost counts a chunk's non-zero slots by host.
func perHost(c *testharness.Cluster, i int, ch meta.Chunk) map[meta.NodeIdx]int {
	out := map[meta.NodeIdx]int{}
	for _, s := range ch.Placement {
		if s != 0 {
			out[hostOf(c, i, s)]++
		}
	}
	return out
}

// One machine, its own store plus four added disks, a pinned 2+2: a disk that fails
// while its host runs is dead at once, reads decode from the others, and after
// fill_after the repairer re-places its slots onto the spare disk.
// Invariant: I-90
// Lifecycle: L-192
func TestCluster_SingleHostDisks_DiskFails_ReadsAndRepair(t *testing.T) {
	c := testharness.NewCluster(t, 1, 2, 2)
	defer c.Close()
	disks := []meta.NodeIdx{c.AddDisk(0), c.AddDisk(0), c.AddDisk(0), c.AddDisk(0)}
	data := testharness.Pattern(3*int(testharness.ChunkSize), 5)
	c.WriteFile(0, "/f", data)
	patch := bytes.Repeat([]byte{0xAB}, 300)
	overwriteAt(t, c, 0, "/f", int64(testharness.ChunkSize)+100, patch)
	copy(data[int(testharness.ChunkSize)+100:], patch)
	chunks := c.ChunksOf(0, "/f")
	for _, ch := range chunks {
		seen := map[meta.NodeIdx]bool{}
		for _, s := range ch.Placement {
			if s == 0 || seen[s] {
				t.Fatalf("placement %v: want 4 distinct targets", ch.Placement)
			}
			seen[s] = true
		}
	}
	// fail a disk that holds a slot of the first chunk
	var victim meta.NodeIdx
	for _, s := range chunks[0].Placement {
		for _, d := range disks {
			if s == d {
				victim = d
			}
		}
	}
	if victim == 0 {
		t.Fatalf("no added disk in %v", chunks[0].Placement)
	}
	c.FailDisk(0, victim, true)
	c.Probe()
	nd := c.Nodes[0]
	if nd.Prober.IsLive(victim) || !nd.Prober.IsLive(nd.Idx()) {
		t.Fatal("failed disk live, or its host dead")
	}
	got, err := c.ReadFileCold(0, "/f")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read with a failed disk: %v", err)
	}
	// inside fill_after: nothing moves; after it: re-placed onto the spare
	st, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired != 0 {
		t.Fatalf("repaired inside fill_after: %+v", st)
	}
	c.Clk.Advance(repairCfg().FillAfter + time.Second)
	c.Probe()
	st, err = newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired == 0 || st.Unrepairable != 0 {
		t.Fatalf("scan after fill_after: %+v", st)
	}
	for _, ch := range c.ChunksOf(0, "/f") {
		for _, s := range ch.Placement {
			if s == 0 || s == victim {
				t.Fatalf("placement %v still uses the failed disk or a hole", ch.Placement)
			}
		}
	}
	got, err = c.ReadFileCold(0, "/f")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after repair: %v", err)
	}
}

// One machine with three added disks in auto mode settles on 2+2 (docs/14 §6).
// Invariant: I-88
func TestCluster_AutoPolicy_SingleHostDisks(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0, testharness.AutoPolicy())
	defer c.Close()
	c.AddDisk(0)
	c.AddDisk(0)
	c.AddDisk(0)
	c.Probe()
	_, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if v := c.Nodes[0].Node.FSM.Volume(); v.K != 2 || v.M != 2 {
		t.Fatalf("auto policy %d+%d, want 2+2", v.K, v.M)
	}
	data := testharness.Pattern(2*int(testharness.ChunkSize), 9)
	c.WriteFile(0, "/g", data)
	for _, ch := range c.ChunksOf(0, "/g") {
		if ch.K != 2 || ch.M != 2 || len(perHost(c, 0, ch)) != 1 {
			t.Fatalf("chunk %d+%d %v", ch.K, ch.M, ch.Placement)
		}
	}
}

// Two hosts at 2+1: chunks written while the second host was draining all sit on the
// first host's targets. Once it is active again, repair moves the slot above the cap
// (ceil(3/2) = 2) to it, and never leaves a chunk with fewer than k readable slots.
// Invariant: I-89
// Lifecycle: L-193
func TestRepair_DomainSkew_MovesWithinCap(t *testing.T) {
	c := testharness.NewCluster(t, 2, 2, 1)
	defer c.Close()
	c.AddDisk(0)
	c.AddDisk(0)
	other := c.Nodes[1].Idx()
	leader := c.Leader()
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: other, State: meta.Draining}))
	c.Settle()
	data := testharness.Pattern(4*int(testharness.ChunkSize), 3)
	c.WriteFile(0, "/skew", data)
	for _, ch := range c.ChunksOf(0, "/skew") {
		if ph := perHost(c, 0, ch); ph[c.Nodes[0].Idx()] != 3 {
			t.Fatalf("setup: placement %v not all on host 0", ch.Placement)
		}
	}
	// while host 1 drains, the skew cannot be fixed: nothing is proposed (L-194)
	st, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired != 0 || st.Partial != 0 {
		t.Fatalf("moved with no room: %+v", st)
	}
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: other, State: meta.Active}))
	c.Settle()
	c.Probe()
	st, err = newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired != 4 {
		t.Fatalf("scan %+v, want 4 chunks moved", st)
	}
	for _, ch := range c.ChunksOf(0, "/skew") {
		ph := perHost(c, 0, ch)
		if ph[c.Nodes[0].Idx()] != 2 || ph[other] != 1 {
			t.Fatalf("after repair: %v by host %v", ch.Placement, ph)
		}
	}
	got, err := c.ReadFileCold(1, "/skew")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after the move: %v", err)
	}
	// a second pass finds nothing to do
	st, err = newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired != 0 || st.Partial != 0 {
		t.Fatalf("second pass moved again: %+v", st)
	}
}

// One host, several disks: there is no other host, so nothing is ever skewed.
// Invariant: I-89
// Lifecycle: L-194
func TestRepair_DomainSkew_UnfixableIgnored(t *testing.T) {
	c := testharness.NewCluster(t, 1, 2, 1)
	defer c.Close()
	c.AddDisk(0)
	c.AddDisk(0)
	c.WriteFile(0, "/one", testharness.Pattern(2*int(testharness.ChunkSize), 1))
	st, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired != 0 || st.Partial != 0 {
		t.Fatalf("single host repaired: %+v", st)
	}
}

// Two hosts of three targets each at 2+2 (cap 2): a dead disk's slots are re-placed so
// that neither host ever holds more than two slots of a stripe.
// Invariant: I-89
// Lifecycle: L-196
func TestRepair_DeadDisk_RePlacesWithinCap(t *testing.T) {
	c := testharness.NewCluster(t, 2, 2, 2)
	defer c.Close()
	a1, a2 := c.AddDisk(0), c.AddDisk(0)
	c.AddDisk(1)
	c.AddDisk(1)
	c.WriteFile(0, "/cap", testharness.Pattern(6*int(testharness.ChunkSize), 4))
	check := func(when string) {
		for _, ch := range c.ChunksOf(0, "/cap") {
			for host, n := range perHost(c, 0, ch) {
				if n > 2 {
					t.Fatalf("%s: host %d holds %d slots of %v", when, host, n, ch.Placement)
				}
			}
		}
	}
	check("written")
	victim := a1
	for _, ch := range c.ChunksOf(0, "/cap") {
		for _, s := range ch.Placement {
			if s == a2 {
				victim = a2
			}
		}
	}
	c.FailDisk(0, victim, true)
	c.Probe()
	c.Clk.Advance(repairCfg().FillAfter + time.Second)
	c.Probe()
	_, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	check("repaired")
	for _, ch := range c.ChunksOf(0, "/cap") {
		for _, s := range ch.Placement {
			if s == victim {
				t.Fatalf("dead disk still placed: %v", ch.Placement)
			}
		}
	}
}

// Draining a host drains its disks too: repair moves every slot off the host's own
// store and its disks, after which the host can be removed (disks first).
// Invariant: I-42
func TestRepair_DrainHost_MovesItsDisks(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	d := c.AddDisk(2)
	c.AddDisk(0)
	c.WriteFile(0, "/drain", testharness.Pattern(6*int(testharness.ChunkSize), 6))
	host := c.Nodes[2].Idx()
	paths := []string{"/drain"}
	onHost := func() int {
		n := 0
		for _, p := range paths {
			for _, ch := range c.ChunksOf(0, p) {
				n += perHost(c, 0, ch)[host]
			}
		}
		return n
	}
	for i := 0; onHost() == 0; i++ { // never skip: more files until the host holds a slot
		if i == 4 {
			t.Fatalf("no chunk of %d six-chunk files placed a slot on host 2", i+1)
		}
		p := fmt.Sprintf("/drain%d", i)
		c.WriteFile(0, p, testharness.Pattern(6*int(testharness.ChunkSize), byte(7+i)))
		paths = append(paths, p)
	}
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: host, State: meta.Draining}))
	c.Settle()
	for pass := 0; pass < 3 && onHost() > 0; pass++ {
		_, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
		must(t, err)
		c.Settle()
	}
	if n := onHost(); n != 0 {
		t.Fatalf("%d slots still on the draining host or its disk", n)
	}
	// removal goes disk first, then host
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: d}))
	c.Settle()
	must(t, c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: host}))
}

var _ = cluster.LiveThreshold
