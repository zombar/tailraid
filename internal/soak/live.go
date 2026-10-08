package soak

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/zombar/tailraid/internal/rpc"
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
}

// NewLiveEnv wires the admin surface to this device's node.
func NewLiveEnv(client *rpc.Client, selfAddr, mountPath, clusterID string, destructive, restarts bool) *LiveEnv {
	return &LiveEnv{Admin: Admin{Caller: client, Seeds: []string{selfAddr}}, SelfAddr: selfAddr, MountPath: mountPath, ClusterID: clusterID, Destructive: destructive, Restarts: restarts}
}

func (e *LiveEnv) Name() string { return "live cluster " + e.ClusterID }

func (e *LiveEnv) Caps() Caps {
	return Caps{Corrupt: e.Destructive, Kill: e.Destructive && e.Restarts, Partition: e.Destructive && e.Restarts}
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
	return out, nil
}

func (e *LiveEnv) Volume(n NodeRef) (Volume, bool) {
	if n.RPC != e.SelfAddr {
		return nil, false
	}
	return OSVolume{Root: e.MountPath}, true
}

func (e *LiveEnv) Corrupt(ctx context.Context, n NodeRef, key string) error {
	if !e.Destructive {
		return errors.New("corruption needs --destructive")
	}
	return e.Admin.Corrupt(ctx, n, key)
}

var errNoFaults = errors.New("live environment cannot kill, restart or partition nodes without --destructive --restarts (docs/12 §3)")

// Kill asks the node to exit; its service manager restarts it (docs/12 §3).
func (e *LiveEnv) Kill(ctx context.Context, n NodeRef) error {
	if !e.Caps().Kill {
		return errNoFaults
	}
	return e.Exit(ctx, n)
}

// Start waits for the service manager's restart to answer /health.
func (e *LiveEnv) Start(ctx context.Context, n NodeRef) error {
	if !e.Caps().Kill {
		return errNoFaults
	}
	return e.AwaitHealth(ctx, n, 2*time.Minute)
}

func (e *LiveEnv) Restart(ctx context.Context, nodes []NodeRef) error {
	for _, n := range nodes {
		if err := e.Kill(ctx, n); err != nil {
			return err
		}
	}
	if err := e.Wait(ctx, 2*time.Second); err != nil {
		return err
	}
	for _, n := range nodes {
		if err := e.Start(ctx, n); err != nil {
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

// Partition installs drop rules on every node of each side for the other side.
func (e *LiveEnv) Partition(ctx context.Context, a, b []NodeRef) error {
	if !e.Caps().Partition {
		return errNoFaults
	}
	ips := func(side []NodeRef) []string {
		out := make([]string, 0, len(side))
		for _, n := range side {
			out = append(out, hostOf(n.RPC))
		}
		return out
	}
	for _, n := range a {
		if err := e.SetFault(ctx, n, ips(b)); err != nil {
			return err
		}
	}
	for _, n := range b {
		if err := e.SetFault(ctx, n, ips(a)); err != nil {
			return err
		}
	}
	return nil
}

// Heal clears every node's rules (best effort: a node mid-restart is retried by Start).
func (e *LiveEnv) Heal(ctx context.Context) error {
	if !e.Caps().Partition {
		return nil
	}
	nodes, err := e.Nodes(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, n := range nodes {
		if err := e.SetFault(ctx, n, nil); err != nil && first == nil {
			first = err
		}
	}
	return first
}
