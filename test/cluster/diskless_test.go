package cluster_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// slotsOnHost counts the slots of every chunk of path that name a disk of host i, or
// the host's own index (which must never happen).
func slotsOnHost(c *testharness.Cluster, i int, path string) int {
	n := 0
	for _, ch := range c.ChunksOf(0, path) {
		for _, s := range ch.Placement {
			if s == c.Nodes[i].Idx() || (s != 0 && c.HostOf(s) == i) {
				n++
			}
		}
	}
	return n
}

// A voter with no disks writes and reads the volume like any member: its writes land on
// the other hosts' disks, its own index never appears in a placement, and it stays a
// voter (docs/14 §9).
// Invariant: I-118
func TestCluster_DisklessVoter_WritesLandOnOtherHosts(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1, testharness.Diskless(0))
	defer c.Close()
	if !c.Voter(0) || c.Nodes[0].Target() != 0 {
		t.Fatal("test needs a diskless voter at n0")
	}
	data := testharness.Pattern(3*int(testharness.ChunkSize)+77, 41)
	c.WriteFile(0, "/f", data)
	c.Settle()
	chunks := c.ChunksOf(0, "/f")
	if len(chunks) != 4 {
		t.Fatalf("chunks %d", len(chunks))
	}
	for _, ch := range chunks {
		for _, s := range ch.Placement {
			if s == 0 {
				t.Fatalf("degraded placement %v", ch.Placement)
			}
		}
	}
	if n := slotsOnHost(c, 0, "/f"); n != 0 {
		t.Fatalf("%d slots on the diskless host", n)
	}
	if n := c.Leader().Node.SlotsHeldBy(c.Nodes[0].Idx()); n != 0 {
		t.Fatalf("SlotsHeldBy(device) = %d", n)
	}
	for i := range c.Nodes {
		if got, err := c.ReadFileCold(i, "/f"); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n%d read: %v", i, err)
		}
	}
	// a diskless node joining later is the same: a member, a gateway, never a target
	j := c.AddDisklessNode(true)
	c.WriteFile(j, "/g", data[:testharness.ChunkSize+5])
	c.Settle()
	if n := slotsOnHost(c, j, "/g"); n != 0 {
		t.Fatalf("%d slots on the joined diskless host", n)
	}
	if got, err := c.ReadFileCold(j, "/g"); err != nil || !bytes.Equal(got, data[:testharness.ChunkSize+5]) {
		t.Fatalf("joined node read: %v", err)
	}
	for _, nd := range c.Leader().Node.FSM.Nodes() {
		if nd.Kind == meta.KindDevice && nd.Capacity != 0 {
			t.Fatalf("device row carries a budget: %+v", nd)
		}
	}
}

// Removing a host's last disk (drained first, so the disk holds no slot) leaves the host
// an Active member and gateway with no target: later writes avoid it and it still reads
// everything (docs/14 §9).
// Invariant: I-118
func TestCluster_RemoveLastDisk_HostBecomesDiskless(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	data := testharness.Pattern(2*int(testharness.ChunkSize), 42)
	c.WriteFile(0, "/f", data)
	c.Settle()
	leader := c.Leader()
	victim := 3
	if leader.I == 3 {
		victim = 2
	}
	disk := c.Nodes[victim].Target()
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: disk, State: meta.Draining}))
	c.Settle()
	for pass := 0; pass < 3 && leader.Node.SlotsHeldBy(disk) > 0; pass++ {
		_, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
		must(t, err)
		c.Settle()
	}
	if n := leader.Node.SlotsHeldBy(disk); n != 0 {
		t.Fatalf("%d slots left on the drained disk", n)
	}
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "remove", NodeIdx: disk}))
	c.Settle()
	host, ok := leader.Node.FSM.Node(c.Nodes[victim].Idx())
	if !ok || host.State != meta.Active || host.Kind != meta.KindDevice {
		t.Fatalf("host after its last disk went: %+v", host)
	}
	for _, nd := range leader.Node.FSM.Nodes() {
		if nd.Kind == meta.KindDisk && nd.Host == host.Idx && nd.State != meta.Removed {
			t.Fatalf("host still has a disk: %+v", nd)
		}
	}
	// the diskless host writes and reads; nothing lands on it
	c.WriteFile(victim, "/g", data[:testharness.ChunkSize+9])
	c.Settle()
	if n := slotsOnHost(c, victim, "/g"); n != 0 {
		t.Fatalf("%d slots on the now diskless host", n)
	}
	for _, path := range []string{"/f", "/g"} {
		want := data
		if path == "/g" {
			want = data[:testharness.ChunkSize+9]
		}
		if got, err := c.ReadFileCold(victim, path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("diskless host read %s: %v", path, err)
		}
	}
}

// set_capacity names a disk: a device row has no budget (docs/14 §9).
// Invariant: I-118
func TestMemberOp_SetCapacity_RefusesDevice(t *testing.T) {
	c := testharness.NewCluster(t, 2, 1, 1)
	defer c.Close()
	leader := c.Leader()
	cases := []struct {
		name string
		idx  meta.NodeIdx
		want error
	}{
		{"a device", c.Nodes[1].Idx(), cluster.ErrBadMemberOp},
		{"the leader's own device", leader.Idx(), cluster.ErrBadMemberOp},
		{"a disk", c.Nodes[1].Target(), nil},
	}
	for _, tc := range cases {
		err := leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_capacity", NodeIdx: tc.idx, Capacity: 1 << 30})
		if !errors.Is(err, tc.want) {
			t.Errorf("set_capacity on %s: %v, want %v", tc.name, err, tc.want)
		}
	}
	c.Settle()
	for _, nd := range leader.Node.FSM.Nodes() {
		if nd.Kind == meta.KindDevice && nd.Capacity != 0 {
			t.Fatalf("device row gained a budget: %+v", nd)
		}
		if nd.Idx == c.Nodes[1].Target() && nd.Capacity != 1<<30 {
			t.Fatalf("disk budget not set: %+v", nd)
		}
	}
}

// A cluster whose only member has no disk has nowhere to put a chunk: the flush fails
// with ErrFull and the client sees ENOSPC once the WAL is at its cap, never EIO
// (docs/14 §9).
// Invariant: I-120
// Lifecycle: L-189
func TestCluster_Diskless_WriteIsENOSPC(t *testing.T) {
	c := testharness.NewCluster(t, 1, 1, 0, testharness.Diskless(0), testharness.WALCap(1000))
	defer c.Close()
	nd := c.Nodes[0]
	write := func(n int) error {
		fh, err := nd.FS.OpenFile("/a", os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		defer fh.Close()
		_, err = fh.Write(testharness.Pattern(n, byte(n)))
		return err
	}
	must(t, write(900))
	err := nd.Flush.FlushAll(context.Background())
	if !errors.Is(err, chunk.ErrFull) || !errors.Is(err, vfs.ErrTooFewAcks) {
		t.Fatalf("flush with no targets: %v, want ErrFull", err)
	}
	must(t, write(900)) // the WAL reaches its cap
	err = write(1)
	if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, chunk.ErrFull) || errors.Is(err, vfs.ErrIO) {
		t.Fatalf("write at the cap: %v, want ENOSPC", err)
	}
	// a disk arriving makes the same data flush
	c.AddDisk(0)
	must(t, nd.Flush.FlushAll(context.Background()))
	if got, err := c.ReadFileCold(0, "/a"); err != nil || len(got) != 900 {
		t.Fatalf("read after a disk arrived: %v (%d bytes)", err, len(got))
	}
}
