package testharness

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
)

// harnessDisk is one added disk of a harness node (docs/14): its own shard store,
// served under /v1/disks/{id}, and a switch that closes it as an unmounted disk would.
type harnessDisk struct {
	id     string
	idx    meta.NodeIdx
	store  *chunk.Store
	failed bool
}

// nodeDisks is a node's added disks; it survives the node's restarts like its data dir.
type nodeDisks struct {
	mu    sync.Mutex
	disks []*harnessDisk
}

func (d *nodeDisks) lookup(id string) (rpc.ShardStore, meta.NodeIdx, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, hd := range d.disks {
		if hd.id == id && !hd.failed {
			return hd.store, hd.idx, true
		}
	}
	return nil, 0, false
}

func (d *nodeDisks) local(id string) rpc.ShardStore {
	if s, _, ok := d.lookup(id); ok {
		return s
	}
	return nil
}

func (d *nodeDisks) health() []rpc.DiskHealth {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]rpc.DiskHealth, 0, len(d.disks))
	for _, hd := range d.disks {
		h := rpc.DiskHealth{ID: hd.id, OK: !hd.failed}
		h.FS, _ = sysfs.FilesystemID(hd.store.Root())
		if hd.failed {
			h.Reason = "failed (harness)"
		} else {
			h.Used, h.Capacity = hd.store.Used(), hd.store.Capacity()
			h.Free = h.Capacity - h.Used
		}
		out = append(out, h)
	}
	return out
}

// AddDisk gives node i one more disk, registered through the leader with the add_disk
// member op, and returns its target index. Every node probes once afterwards so the disk
// is live everywhere. A node's first disk is its default disk: its Store (docs/14 §9).
func (c *Cluster) AddDisk(i int) meta.NodeIdx {
	c.T.Helper()
	nd := c.Nodes[i]
	nd.disks.mu.Lock()
	n := len(nd.disks.disks) + 1
	nd.disks.mu.Unlock()
	id := fmt.Sprintf("%08x%08x", i+1, n)
	store, err := chunk.Open(sysfs.OS{}, filepath.Join(nd.Dir, fmt.Sprintf("disk%d", n), "shards"), 1<<40)
	if err != nil {
		c.T.Fatal(err)
	}
	hd := &harnessDisk{id: id, store: store}
	nd.disks.mu.Lock()
	nd.disks.disks = append(nd.disks.disks, hd)
	nd.disks.mu.Unlock()
	op := rpc.MemberOp{Op: "add_disk", NodeIdx: nd.Idx(), DiskID: id, Name: fmt.Sprintf("n%d/%d", i, n), Capacity: 1 << 40}
	if err := c.Leader().Node.MemberOp(context.Background(), op); err != nil {
		c.T.Fatalf("add disk to n%d: %v", i, err)
	}
	c.Settle()
	row, ok := nd.Node.FSM.NodeByID(id)
	if !ok {
		c.T.Fatalf("disk %s not in n%d's node table", id, i)
	}
	nd.disks.mu.Lock()
	hd.idx = row.Idx
	if n == 1 {
		nd.Store = store
	}
	nd.disks.mu.Unlock()
	c.Probe()
	return row.Idx
}

// StoreOf is the shard store behind placement target s (a disk), or nil.
func (c *Cluster) StoreOf(s meta.NodeIdx) *chunk.Store {
	st, _ := c.Target(s)
	return st
}

// HostOf is the index (in c.Nodes) of the node that hosts target s, or -1.
func (c *Cluster) HostOf(s meta.NodeIdx) int {
	_, i := c.Target(s)
	return i
}

// FailDisk closes (failed) or reopens one of node i's added disks: its host keeps
// running and reports the disk not OK, as it would an unmounted or failing disk.
func (c *Cluster) FailDisk(i int, disk meta.NodeIdx, failed bool) {
	c.T.Helper()
	nd := c.Nodes[i]
	nd.disks.mu.Lock()
	defer nd.disks.mu.Unlock()
	for _, hd := range nd.disks.disks {
		if hd.idx == disk {
			hd.failed = failed
			return
		}
	}
	c.T.Fatalf("n%d has no disk %d", i, disk)
}

// DiskStore is the shard store of one of node i's added disks.
func (c *Cluster) DiskStore(i int, disk meta.NodeIdx) *chunk.Store {
	nd := c.Nodes[i]
	nd.disks.mu.Lock()
	defer nd.disks.mu.Unlock()
	for _, hd := range nd.disks.disks {
		if hd.idx == disk {
			return hd.store
		}
	}
	return nil
}

// Target resolves a placement target (a disk) to its shard store and the index (in
// c.Nodes) of the node that hosts it.
func (c *Cluster) Target(s meta.NodeIdx) (*chunk.Store, int) {
	for i, nd := range c.Nodes {
		nd.disks.mu.Lock()
		for _, hd := range nd.disks.disks {
			if hd.idx == s {
				nd.disks.mu.Unlock()
				return hd.store, i
			}
		}
		nd.disks.mu.Unlock()
	}
	return nil, -1
}

// PlacedOn answers the DELETE fence for an added disk (rpc.DiskFence).
func (f fence) PlacedOn(target meta.NodeIdx, key chunk.ShardKey) bool {
	h, k, m, idx, err := chunk.ParseKey(key)
	if err != nil {
		return false
	}
	c, ok := f.nd.Node.FSM.GetChunk(h)
	return ok && c.K == k && c.M == m && idx < len(c.Placement) && c.Placement[idx] == target
}
