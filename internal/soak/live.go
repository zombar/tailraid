package soak

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
)

// LiveEnv is the user's real cluster seen from one device (`tailraid verify`, docs/09 §6):
// the device's own mount for IO, its tailnet identity for RPC. Kills and partitions are
// not available (they need a restarting service manager, M4); corruption is, with
// Destructive, when the targets run with dev.enabled.
type LiveEnv struct {
	Admin
	SelfAddr    string // this device's RPC address
	MountPath   string
	ClusterID   string
	Destructive bool
	Restarts    bool // every target runs with dev.enabled under a restarting service manager

	all    []NodeRef // the members as of Nodes
	faults faultTable
}

// NewLiveEnv wires the admin surface to this device's node.
func NewLiveEnv(client *rpc.Client, selfAddr, mountPath, clusterID string, destructive, restarts bool) *LiveEnv {
	return &LiveEnv{Caller: client, Seeds: []string{selfAddr}, SelfAddr: selfAddr, MountPath: mountPath, ClusterID: clusterID, Destructive: destructive, Restarts: restarts}
}

func (e *LiveEnv) Name() string { return "live cluster " + e.ClusterID }

func (e *LiveEnv) Caps() Caps {
	return Caps{Corrupt: e.Destructive, Mutate: e.Destructive, Kill: e.Destructive && e.Restarts, Partition: e.Destructive && e.Restarts}
}

// Nodes lists the members from this device's status; the local node comes first so its
// gateway is the one the run writes through.
func (e *LiveEnv) Nodes(ctx context.Context) ([]NodeRef, error) {
	rep, err := e.SeedStatus(ctx)
	if err != nil {
		return nil, err
	}
	var out []NodeRef
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if n.Kind != "device" || n.State == "removed" {
			continue
		}
		ref := NodeRef{Name: n.Name, Idx: n.Idx, RPC: n.RPCAddr()}
		if n.Self {
			out = append([]NodeRef{ref}, out...)
		} else {
			out = append(out, ref)
		}
	}
	e.all = out
	return out, nil
}

// FailDisk and RestoreDisk are not offered on a live cluster: closing a disk means
// unmounting it on its device, which the operator does by hand (Caps.Disks is false).
func (e *LiveEnv) FailDisk(context.Context, NodeRef) error {
	return errors.New("live: a disk is closed by unmounting it on its device")
}

func (e *LiveEnv) RestoreDisk(context.Context, NodeRef) error {
	return errors.New("live: a disk is restored by mounting it on its device")
}

func (e *LiveEnv) AddDisk(context.Context, NodeRef) error {
	return errors.New("live: a disk is added with `tailraid node add-disk` on its device")
}

// AddS3 and Bucket are not offered on a live cluster: the bucket is the operator's
// (Caps.S3 is false), and `node add-s3` is run by hand (docs/10 §4.1).
func (e *LiveEnv) AddS3(context.Context, string) error {
	return errors.New("live: a bucket is added with `tailraid node add-s3` on a device")
}

func (e *LiveEnv) Bucket(context.Context) (*s3.Bucket, error) {
	return nil, errors.New("live: no bucket for the soak")
}

func (e *LiveEnv) Volume(n NodeRef) (Volume, bool) {
	if n.RPC != e.SelfAddr {
		return nil, false
	}
	return OSVolume{Root: e.MountPath, Transient: TransientNetError, RetryFor: 30 * time.Second}, true
}

func (e *LiveEnv) Corrupt(ctx context.Context, n NodeRef, key string) error {
	if !e.Destructive {
		return errors.New("corruption needs --destructive")
	}
	return e.Admin.Corrupt(ctx, n, key)
}

var errNoFaults = errors.New("live environment cannot kill, restart or partition nodes without --destructive --restarts (docs/12 §3)")

// faultTable composes every node's /dev/fault drop rules from two sources: nodes held
// down after a kill (isolated from everyone, since the service manager bounces the
// process within seconds) and the current partition.
type faultTable struct {
	held map[meta.NodeIdx]bool
	side map[meta.NodeIdx]int // 0: not partitioned
}

func (t *faultTable) hold(n NodeRef, on bool) {
	if t.held == nil {
		t.held = map[meta.NodeIdx]bool{}
	}
	t.held[n.Idx] = on
}

func (t *faultTable) partition(a, b []NodeRef) {
	t.side = map[meta.NodeIdx]int{}
	for _, n := range a {
		t.side[n.Idx] = 1
	}
	for _, n := range b {
		t.side[n.Idx] = 2
	}
}

// rules lists the peers n must drop: every peer while n or the peer is held down, and the
// other side of the partition. The device running verify (keep, its RPC address) is never
// dropped: the driver's calls come from it and must still reach a held-down node to lift
// its rules, and its own node drops the other side in both directions, which cuts the
// link on its own (docs/12 §3).
func (t *faultTable) rules(n NodeRef, all []NodeRef, keep string) []string {
	var out []string
	for _, o := range all {
		if o.Idx == n.Idx || (keep != "" && o.RPC == keep) {
			continue
		}
		cut := t.held[n.Idx] || t.held[o.Idx] || (t.side[n.Idx] != 0 && t.side[o.Idx] != 0 && t.side[n.Idx] != t.side[o.Idx])
		if cut {
			out = append(out, hostOf(o.RPC))
		}
	}
	return out
}

// applyFaults pushes every node's rules; nodes in skip are left for the caller.
func (e *LiveEnv) applyFaults(ctx context.Context, skip ...NodeRef) error {
	var first error
	for _, n := range e.all {
		skipped := false
		for _, s := range skip {
			skipped = skipped || s.Idx == n.Idx
		}
		if skipped {
			continue
		}
		if err := e.SetFault(ctx, n, e.faults.rules(n, e.all, e.SelfAddr)); err != nil && first == nil {
			first = fmt.Errorf("fault rules on %s: %w", n.Name, err)
		}
	}
	return first
}

// Kill makes a node dead to the cluster for as long as the phase wants: its peers drop it,
// the process exits (/dev/exit) and, once the service manager has restarted it, it drops
// every peer itself. Start lifts the rules. The device running verify is never killed:
// its mount is the run's only volume.
func (e *LiveEnv) Kill(ctx context.Context, n NodeRef) error {
	if !e.Caps().Kill {
		return errNoFaults
	}
	if n.RPC == e.SelfAddr {
		return fmt.Errorf("%s runs verify; it cannot kill its own node", n.Name)
	}
	e.faults.hold(n, true)
	if err := e.applyFaults(ctx, n); err != nil {
		return err
	}
	if err := e.Exit(ctx, n); err != nil {
		return err
	}
	if err := e.Wait(ctx, time.Second); err != nil { // the old process is gone after 100 ms
		return err
	}
	if err := e.AwaitHealth(ctx, n, 2*time.Minute); err != nil {
		return err
	}
	return e.SetFault(ctx, n, e.faults.rules(n, e.all, e.SelfAddr))
}

// Start lifts a kill's hold-down (the process is already back) and waits for /health.
func (e *LiveEnv) Start(ctx context.Context, n NodeRef) error {
	if !e.Caps().Kill {
		return errNoFaults
	}
	e.faults.hold(n, false)
	if err := e.applyFaults(ctx); err != nil {
		return err
	}
	return e.AwaitHealth(ctx, n, 2*time.Minute)
}

// Restart bounces every node through its service manager, this device included (the
// kernel NFS client rides out its gateway's restart).
func (e *LiveEnv) Restart(ctx context.Context, nodes []NodeRef) error {
	if !e.Caps().Kill {
		return errNoFaults
	}
	for _, n := range nodes {
		if err := e.Exit(ctx, n); err != nil {
			return err
		}
	}
	if err := e.Wait(ctx, 2*time.Second); err != nil {
		return err
	}
	for _, n := range nodes {
		if err := e.AwaitHealth(ctx, n, 2*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

func hostOf(rpcAddr string) string {
	if h, _, err := net.SplitHostPort(rpcAddr); err == nil {
		return h
	}
	return rpcAddr
}

// Partition installs the drop rules of each side for the other.
func (e *LiveEnv) Partition(ctx context.Context, a, b []NodeRef) error {
	if !e.Caps().Partition {
		return errNoFaults
	}
	e.faults.partition(a, b)
	return e.applyFaults(ctx)
}

// Heal lifts the partition; nodes held down by a kill stay held until Start.
func (e *LiveEnv) Heal(ctx context.Context) error {
	if !e.Caps().Partition {
		return nil
	}
	e.faults.partition(nil, nil)
	return e.applyFaults(ctx)
}
