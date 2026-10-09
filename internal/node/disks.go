package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
)

// MarkerName is the disk marker file in an added disk's root (docs/14 §2).
const MarkerName = "disk_id"

// diskMarker names the cluster, the disk and the host a disk belongs to. A disk whose
// marker does not match this cluster and this host is never opened (I-86).
type diskMarker struct {
	ClusterID string `json:"cluster_id"`
	DiskID    string `json:"disk_id"`
	HostID    string `json:"host_id"`
}

// ErrDiskNotEmpty refuses add-disk on a path that already holds data.
var ErrDiskNotEmpty = errors.New("node: disk path is not empty")

// PrepareDisk marks an empty directory as a disk of this host and returns its new disk
// ID: the marker is written and fsynced before the disk is registered, and an existing
// marker or any other content refuses the path (docs/14 §2). lost+found is allowed.
func PrepareDisk(path string, clusterID [16]byte, hostID string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if e.Name() != "lost+found" {
			return "", fmt.Errorf("%w: %s holds %s", ErrDiskNotEmpty, path, e.Name())
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	data, _ := json.Marshal(diskMarker{ClusterID: hex.EncodeToString(clusterID[:]), DiskID: id, HostID: hostID})
	f, err := os.OpenFile(filepath.Join(path, MarkerName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return id, sysfs.OS{}.SyncDir(path)
}

// checkDisk decides whether the disk at path may be opened as one of this host's
// targets: its marker must name this cluster and this host, and the node table must hold
// a live (not Removed) disk row for it under this host. reason says why not (I-86).
func checkDisk(path string, clusterID [16]byte, hostID string, host meta.NodeIdx, nodes []meta.Node) (row meta.Node, reason string) {
	b, err := os.ReadFile(filepath.Join(path, MarkerName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return meta.Node{}, "disk_id missing — not mounted?"
	case err != nil:
		return meta.Node{}, "disk_id unreadable: " + err.Error()
	}
	var m diskMarker
	if err := json.Unmarshal(b, &m); err != nil || m.DiskID == "" {
		return meta.Node{}, "disk_id unreadable"
	}
	if m.ClusterID != hex.EncodeToString(clusterID[:]) {
		return meta.Node{}, "disk_id belongs to another cluster"
	}
	if m.HostID != hostID {
		return meta.Node{}, "disk_id belongs to another host"
	}
	for i := range nodes {
		if n := nodes[i]; n.Kind == meta.KindDisk && n.ID == m.DiskID {
			switch {
			case n.Host != host:
				return meta.Node{}, "disk is registered under another host"
			case n.State == meta.Removed:
				return meta.Node{}, "disk was removed from the cluster"
			}
			return n, ""
		}
	}
	return meta.Node{}, "disk not registered (run `tailraid node add-disk`)"
}

// localDisk is one configured disk of this host: open (store set) or closed (reason).
type localDisk struct {
	cfg    config.Disk
	idx    meta.NodeIdx
	id     string
	store  *chunk.Store
	reason string
}

// diskSet is this host's added disks (docs/14 §3). Disks open once their marker checks
// out and stay open; a closed disk is retried every maintenance tick, so a disk that was
// added while serve ran, or mounted late, comes up without a restart.
type diskSet struct {
	clusterID [16]byte
	hostID    string
	host      meta.NodeIdx
	log       *slog.Logger

	mu    sync.RWMutex
	disks []*localDisk // config order
}

func newDiskSet(cfg config.Config, clusterID [16]byte, hostID string, host meta.NodeIdx, log *slog.Logger) *diskSet {
	d := &diskSet{clusterID: clusterID, hostID: hostID, host: host, log: log}
	for _, c := range cfg.Node.Disks {
		d.disks = append(d.disks, &localDisk{cfg: c, reason: "not opened yet"})
	}
	return d
}

// refresh opens every closed disk whose marker now checks out. A nil set has no disks.
func (d *diskSet) refresh(nodes []meta.Node) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ld := range d.disks {
		if ld.store != nil {
			continue
		}
		row, reason := checkDisk(ld.cfg.Path, d.clusterID, d.hostID, d.host, nodes)
		if reason != "" {
			if reason != ld.reason {
				d.log.Warn("disk closed", "path", ld.cfg.Path, "reason", reason)
			}
			ld.reason = reason
			continue
		}
		store, err := openTargetStore(filepath.Join(ld.cfg.Path, "shards"), ld.cfg.Capacity)
		if err != nil {
			ld.reason = "open: " + err.Error()
			d.log.Warn("disk closed", "path", ld.cfg.Path, "err", err)
			continue
		}
		ld.idx, ld.id, ld.store, ld.reason = row.Idx, row.ID, store, ""
		d.log.Info("disk open", "path", ld.cfg.Path, "disk", row.Name, "idx", row.Idx)
	}
}

// DiskBudget is the budget an added disk registers with: its pinned capacity, or the
// budget derived from its filesystem (docs/14 §3).
func DiskBudget(d config.Disk) (uint64, error) {
	s, err := openTargetStore(filepath.Join(d.Path, "shards"), d.Capacity)
	if err != nil {
		return 0, err
	}
	return uint64(s.Capacity()), nil
}

// DiskName is the n-th (from 1) added disk of host: "<host>/<n>".
func DiskName(host string, n int) string { return fmt.Sprintf("%s/%d", host, n) }

// RegisterDisks marks and registers every disk listed in the config, on a node that has
// just bootstrapped the cluster (init takes the config's disks, docs/14 §2).
func RegisterDisks(ctx context.Context, n *cluster.Node, cfg config.Config, clusterID [16]byte) error {
	self, ok := n.SelfIdx()
	if !ok {
		return errors.New("node: this node is not in its own node table")
	}
	for i, d := range cfg.Node.Disks {
		id, err := PrepareDisk(d.Path, clusterID, n.ID)
		if err != nil {
			return fmt.Errorf("disk %s: %w", d.Path, err)
		}
		budget, err := DiskBudget(d)
		if err != nil {
			return fmt.Errorf("disk %s: %w", d.Path, err)
		}
		disk := meta.Node{ID: id, Name: DiskName(cfg.Node.Name, i+1), Kind: meta.KindDisk, Host: self, Capacity: budget, JoinedAt: time.Now().UnixNano()}
		res, _, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: disk}})
		if err == nil {
			err = res.Err
		}
		if err != nil {
			return fmt.Errorf("disk %s: register: %w", d.Path, err)
		}
	}
	return nil
}

// openTargetStore opens a target's shard store: capacity pins its budget, "" derives it
// from the filesystem (docs/01 §Node budget).
func openTargetStore(root, capacity string) (*chunk.Store, error) {
	var capBytes uint64
	if capacity != "" {
		var err error
		if capBytes, err = config.ParseBytes(capacity); err != nil {
			return nil, err
		}
	}
	store, err := chunk.Open(sysfs.OS{}, root, int64(capBytes))
	if err != nil {
		return nil, err
	}
	if capacity == "" {
		if _, err := store.Rebudget(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// add starts tracking a disk added while serve runs (`node add-disk`); the next refresh
// opens it once its marker and row check out. A path already tracked is ignored.
func (d *diskSet) add(c config.Disk) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ld := range d.disks {
		if filepath.Clean(ld.cfg.Path) == filepath.Clean(c.Path) {
			return
		}
	}
	d.disks = append(d.disks, &localDisk{cfg: c, reason: "not opened yet"})
}

// state is "open", or why the disk at path is closed.
func (d *diskSet) state(path string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, ld := range d.disks {
		if filepath.Clean(ld.cfg.Path) == filepath.Clean(path) {
			if ld.store != nil {
				return "open"
			}
			return ld.reason
		}
	}
	return "not configured"
}

// open lists the open disks.
func (d *diskSet) open() []*localDisk {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []*localDisk
	for _, ld := range d.disks {
		if ld.store != nil {
			out = append(out, ld)
		}
	}
	return out
}

// lookup is the RPC server's view: an open disk by ID, with its target index.
func (d *diskSet) lookup(id string) (rpc.ShardStore, meta.NodeIdx, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, ld := range d.disks {
		if ld.store != nil && ld.id == id {
			return ld.store, ld.idx, true
		}
	}
	return nil, 0, false
}

// local is the RPC client's view: an open disk by ID, or nil (a nil interface, never a
// typed nil) when it is closed.
func (d *diskSet) local(id string) rpc.ShardStore {
	if s, _, ok := d.lookup(id); ok {
		return s
	}
	return nil
}

// health lists every configured disk for /health; a closed one is not OK, which makes
// it dead to every node (I-90). A disk known by its marker but not open reports its ID.
func (d *diskSet) health() []rpc.DiskHealth {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]rpc.DiskHealth, 0, len(d.disks))
	for _, ld := range d.disks {
		h := rpc.DiskHealth{ID: ld.id, OK: ld.store != nil, Reason: ld.reason}
		if ld.store != nil {
			h.Used, h.Capacity = ld.store.Used(), ld.store.Capacity()
			h.Free = max(h.Capacity-h.Used, 0)
		} else if b, err := os.ReadFile(filepath.Join(ld.cfg.Path, MarkerName)); err == nil {
			var m diskMarker
			if json.Unmarshal(b, &m) == nil {
				h.ID = m.DiskID
			}
		}
		out = append(out, h)
	}
	return out
}

// target is one of this host's shard stores with its target index and whether its budget
// is pinned: disk 0 first, then the open disks.
type target struct {
	idx    meta.NodeIdx
	store  *chunk.Store
	pinned bool
}

// budgets derives the budget of every unpinned target, dividing a filesystem that
// several targets share among them (I-91). It returns target index → budget.
func budgets(targets []target) (map[meta.NodeIdx]uint64, error) {
	type group struct {
		used, free, total uint64
		members           []target
	}
	groups := map[string]*group{}
	var order []string
	for _, t := range targets {
		if t.pinned {
			continue
		}
		used, free, total, err := t.store.Disk()
		if err != nil {
			return nil, err
		}
		fsid, err := sysfs.FilesystemID(t.store.Root())
		if err != nil {
			return nil, err
		}
		g := groups[fsid]
		if g == nil {
			g = &group{free: free, total: total}
			groups[fsid] = g
			order = append(order, fsid)
		}
		g.used += used
		g.members = append(g.members, t)
	}
	out := map[meta.NodeIdx]uint64{}
	for _, fsid := range order {
		g := groups[fsid]
		b := chunk.SharedBudget(g.used, g.free, g.total, len(g.members))
		for _, t := range g.members {
			t.store.SetCapacity(int64(b))
			out[t.idx] = b
		}
	}
	return out, nil
}
