package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
)

// LiveEnv is the user's real cluster seen from one device (`tailraid verify`, docs/09 §6):
// the device's own mount for IO, its tailnet identity for RPC. Kills and partitions are
// not available (they need a restarting service manager, M4); corruption is, with
// Destructive, when the targets run with dev.enabled.
type LiveEnv struct {
	Client      *rpc.Client
	SelfAddr    string // this device's RPC address
	MountPath   string
	ClusterID   string
	Destructive bool
	Restarts    bool // every target runs with dev.enabled under a restarting service manager

	selfID string
}

func (e *LiveEnv) Name() string { return "live cluster " + e.ClusterID }

func (e *LiveEnv) Caps() Caps {
	return Caps{Corrupt: e.Destructive, Kill: e.Destructive && e.Restarts, Partition: e.Destructive && e.Restarts}
}

func (e *LiveEnv) Wait(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *LiveEnv) status(ctx context.Context, addr string) (admin.Report, error) {
	var rep admin.Report
	raw, err := e.Client.Status(ctx, addr)
	if err != nil {
		return rep, err
	}
	err = json.Unmarshal(raw, &rep)
	return rep, err
}

func addrOf(n *admin.NodeReport) string {
	return net.JoinHostPort(n.Addr, strconv.Itoa(int(n.RPCPort)))
}

// Nodes lists the members from this device's status; the local node comes first so its
// gateway is the one the run writes through.
func (e *LiveEnv) Nodes(ctx context.Context) ([]NodeRef, error) {
	rep, err := e.status(ctx, e.SelfAddr)
	if err != nil {
		return nil, fmt.Errorf("status from %s (is `tailraid serve` running?): %w", e.SelfAddr, err)
	}
	e.selfID = rep.Node
	var out []NodeRef
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if n.Kind != "device" || n.State == "removed" {
			continue
		}
		ref := NodeRef{Name: n.Name, Idx: n.Idx, RPC: addrOf(n)}
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

func (e *LiveEnv) Status(ctx context.Context, n NodeRef) (admin.Report, error) {
	return e.status(ctx, n.RPC)
}

func (e *LiveEnv) leaderAddr(ctx context.Context) (string, error) {
	rep, err := e.status(ctx, e.SelfAddr)
	if err != nil {
		return "", err
	}
	for i := range rep.Nodes {
		if rep.Nodes[i].Leader {
			return addrOf(&rep.Nodes[i]), nil
		}
	}
	return "", errors.New("no leader")
}

func (e *LiveEnv) leaderOp(ctx context.Context, op rpc.AdminOp, out any) error {
	addr, err := e.leaderAddr(ctx)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 6; attempt++ {
		raw, err := e.Client.Admin(ctx, addr, op)
		if err == nil {
			if out == nil {
				return nil
			}
			return json.Unmarshal(raw, out)
		}
		last = err
		if redirect := rpc.LeaderFromErr(err); redirect != "" { // 421 names the leader
			addr = redirect
			continue
		}
		if werr := e.Wait(ctx, time.Second); werr != nil {
			return werr
		}
		if a, err := e.leaderAddr(ctx); err == nil {
			addr = a
		}
	}
	return last
}

func (e *LiveEnv) RepairNow(ctx context.Context) (repair.Stats, error) {
	var st repair.Stats
	err := e.leaderOp(ctx, rpc.AdminOp{Op: "repair_now"}, &st)
	return st, err
}

func (e *LiveEnv) Scrub(ctx context.Context) (repair.Stats, error) {
	var rep struct {
		Scan repair.Stats `json:"scan"`
	}
	err := e.leaderOp(ctx, rpc.AdminOp{Op: "check", Scrub: true}, &rep)
	return rep.Scan, err
}

func (e *LiveEnv) GCNow(ctx context.Context) (repair.Result, error) {
	var res repair.Result
	err := e.leaderOp(ctx, rpc.AdminOp{Op: "gc_now"}, &res)
	return res, err
}

func (e *LiveEnv) SetPolicy(ctx context.Context, k, m uint8) error {
	return e.leaderOp(ctx, rpc.AdminOp{Op: "set_policy", K: k, M: m}, nil)
}

func (e *LiveEnv) SetState(ctx context.Context, n NodeRef, state meta.NodeState) error {
	addr, err := e.leaderAddr(ctx)
	if err != nil {
		return err
	}
	return e.Client.Members(ctx, addr, rpc.MemberOp{Op: "set_state", NodeIdx: n.Idx, State: state})
}

func (e *LiveEnv) Fsck(ctx context.Context, n NodeRef) (bool, string, error) {
	var rep struct {
		Violations []string `json:"violations"`
		Orphans    int      `json:"orphan_shards_on_this_node"`
		OK         bool     `json:"ok"`
	}
	raw, err := e.Client.Admin(ctx, n.RPC, rpc.AdminOp{Op: "fsck"})
	if err != nil {
		return false, "", err
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return false, "", err
	}
	return rep.OK, fmt.Sprintf("%d violations, %d orphan shards", len(rep.Violations), rep.Orphans), nil
}

func (e *LiveEnv) Chunks(ctx context.Context, n NodeRef, path string) ([]admin.ChunkReport, error) {
	raw, err := e.Client.Admin(ctx, n.RPC, rpc.AdminOp{Op: "chunk", Chunk: path})
	if err != nil {
		return nil, err
	}
	var reps []admin.ChunkReport
	err = json.Unmarshal(raw, &reps)
	return reps, err
}

func (e *LiveEnv) FSMHash(ctx context.Context, n NodeRef) (uint64, string, error) {
	var h struct {
		Applied uint64 `json:"applied"`
		Hash    string `json:"hash"`
	}
	body, err := e.Client.Call(ctx, n.RPC, http.MethodGet, "/dev/fsm-hash", nil)
	if err != nil {
		return 0, "", err
	}
	err = json.Unmarshal(body, &h)
	return h.Applied, h.Hash, err
}

func (e *LiveEnv) Corrupt(ctx context.Context, n NodeRef, key string) error {
	if !e.Destructive {
		return errors.New("corruption needs --destructive")
	}
	_, err := e.Client.Call(ctx, n.RPC, http.MethodPost, "/dev/corrupt?key="+key, nil)
	return err
}

var errNoFaults = errors.New("live environment cannot kill, restart or partition nodes without --destructive --restarts (docs/12 §3)")

// Kill asks the node to exit; its service manager restarts it (docs/12 §3).
func (e *LiveEnv) Kill(ctx context.Context, n NodeRef) error {
	if !e.Caps().Kill {
		return errNoFaults
	}
	_, err := e.Client.Call(ctx, n.RPC, http.MethodPost, "/dev/exit", nil)
	return err
}

// Start waits for the service manager's restart to answer /health.
func (e *LiveEnv) Start(ctx context.Context, n NodeRef) error {
	if !e.Caps().Kill {
		return errNoFaults
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := e.Client.Health(hctx, n.RPC)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not come back: %w", n.Name, err)
		}
		if err := e.Wait(ctx, 2*time.Second); err != nil {
			return err
		}
	}
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

func (e *LiveEnv) setFault(ctx context.Context, n NodeRef, drop []string) error {
	body, _ := json.Marshal(map[string]any{"drop": drop})
	_, err := e.Client.Call(ctx, n.RPC, http.MethodPost, "/dev/fault", body)
	return err
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
		if err := e.setFault(ctx, n, ips(b)); err != nil {
			return err
		}
	}
	for _, n := range b {
		if err := e.setFault(ctx, n, ips(a)); err != nil {
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
		if err := e.setFault(ctx, n, nil); err != nil && first == nil {
			first = err
		}
	}
	return first
}
