package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
)

// Admin is the soak admin surface over the node RPC (docs/08), shared by the environments
// that reach the cluster remotely: status, leader lookup with 421 redirects, the
// maintenance operations and the /dev/* endpoints. The transport is the Caller; Seeds
// are the nodes asked for status, in order, until one answers.
type Admin struct {
	Caller admin.Caller
	Seeds  []string
}

// Wait sleeps in real time (these environments run against real clocks).
func (a *Admin) Wait(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Admin) Status(ctx context.Context, n NodeRef) (admin.Report, error) {
	return admin.FetchStatus(ctx, a.Caller, n.RPC)
}

// SeedStatus is the status report of the first seed that answers.
func (a *Admin) SeedStatus(ctx context.Context) (admin.Report, error) {
	var last error
	for _, addr := range a.Seeds {
		rep, err := admin.FetchStatus(ctx, a.Caller, addr)
		if err == nil {
			return rep, nil
		}
		last = fmt.Errorf("status from %s (is `tailraid serve` running?): %w", addr, err)
	}
	if last == nil {
		last = errors.New("soak: no seed nodes")
	}
	return admin.Report{}, last
}

// LeaderAddr is the leader's RPC address as the seeds see it.
func (a *Admin) LeaderAddr(ctx context.Context) (string, error) {
	rep, err := a.SeedStatus(ctx)
	if err != nil {
		return "", err
	}
	if l := rep.LeaderRPC(); l != "" {
		return l, nil
	}
	return "", errors.New("no leader")
}

func (a *Admin) call(ctx context.Context, addr, method, path string, body []byte, out any) error {
	raw, err := a.Caller.Call(ctx, addr, method, path, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// leaderCall sends a request to the leader, following a 421 redirect and re-resolving the
// leader after any other failure, for a bounded number of attempts.
func (a *Admin) leaderCall(ctx context.Context, path string, body []byte, out any) error {
	addr, err := a.LeaderAddr(ctx)
	if err != nil {
		return err
	}
	var last error
	for range 6 { // the leader may move between lookup and call
		err := a.call(ctx, addr, http.MethodPost, path, body, out)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("via %s: %w", addr, err)
		if redirect := rpc.LeaderFromErr(err); redirect != "" { // the reply names the leader
			addr = redirect
			continue
		}
		if werr := a.Wait(ctx, time.Second); werr != nil {
			return werr
		}
		if l, err := a.LeaderAddr(ctx); err == nil {
			addr = l
		}
	}
	return last
}

func (a *Admin) leaderOp(ctx context.Context, op rpc.AdminOp, out any) error {
	body, _ := json.Marshal(op)
	return a.leaderCall(ctx, "/v1/admin", body, out)
}

func (a *Admin) nodeOp(ctx context.Context, n NodeRef, op rpc.AdminOp, out any) error {
	body, _ := json.Marshal(op)
	return a.call(ctx, n.RPC, http.MethodPost, "/v1/admin", body, out)
}

// PassTimeout bounds an admin op that runs a pass over the live volume (repair, scrub,
// GC): the leader answers when the pass is done, and the caller's deadline is the only
// one (docs/08, I-189), so the environment sets this one.
const PassTimeout = 30 * time.Minute

// passOp runs a pass op on the leader under PassTimeout.
func (a *Admin) passOp(ctx context.Context, op rpc.AdminOp, out any) error {
	ctx, cancel := context.WithTimeout(ctx, PassTimeout)
	defer cancel()
	return a.leaderOp(ctx, op, out)
}

func (a *Admin) RepairNow(ctx context.Context) (repair.Stats, error) {
	var st repair.Stats
	err := a.passOp(ctx, rpc.AdminOp{Op: "repair_now"}, &st)
	return st, err
}

func (a *Admin) Scrub(ctx context.Context) (repair.Stats, error) {
	var rep admin.CheckReport
	err := a.passOp(ctx, rpc.AdminOp{Op: "check", Scrub: true}, &rep)
	return rep.Scan, err
}

func (a *Admin) GCNow(ctx context.Context) (repair.Result, error) {
	var res repair.Result
	err := a.passOp(ctx, rpc.AdminOp{Op: "gc_now"}, &res)
	return res, err
}

func (a *Admin) PurgeTrash(ctx context.Context) (int, error) {
	var res struct {
		Purged int `json:"purged"`
	}
	err := a.leaderOp(ctx, rpc.AdminOp{Op: "trash_purge", All: true}, &res)
	return res.Purged, err
}

func (a *Admin) SetPolicy(ctx context.Context, k, m uint8) error {
	return a.leaderOp(ctx, rpc.AdminOp{Op: "set_policy", K: k, M: m}, nil)
}

func (a *Admin) SetPolicyAuto(ctx context.Context) error {
	return a.leaderOp(ctx, rpc.AdminOp{Op: "set_policy", Auto: true}, nil)
}

// Disks is the node's open disks as their budgets see them (/dev/disk, dev.enabled only).
func (a *Admin) Disks(ctx context.Context, n NodeRef) ([]DiskInfo, error) {
	var body struct {
		Disks []DiskInfo `json:"disks"`
	}
	err := a.call(ctx, n.RPC, http.MethodGet, "/dev/disk", nil, &body)
	return body.Disks, err
}

// Fill makes the node consume bytes of one disk with a scratch file (0 removes it).
func (a *Admin) Fill(ctx context.Context, n NodeRef, disk string, bytes uint64) error {
	return a.call(ctx, n.RPC, http.MethodPost, fmt.Sprintf("/dev/fill?bytes=%d&disk=%s", bytes, disk), nil, nil)
}

func (a *Admin) SetState(ctx context.Context, n NodeRef, state meta.NodeState) error {
	body, _ := json.Marshal(rpc.MemberOp{Op: "set_state", NodeIdx: n.Idx, State: state})
	return a.leaderCall(ctx, "/v1/members", body, nil)
}

func (a *Admin) Remove(ctx context.Context, n NodeRef) error {
	body, _ := json.Marshal(rpc.MemberOp{Op: "remove", NodeIdx: n.Idx})
	return a.leaderCall(ctx, "/v1/members", body, nil)
}

func (a *Admin) Fsck(ctx context.Context, n NodeRef) (bool, int, string, error) {
	var rep admin.FsckReport
	if err := a.nodeOp(ctx, n, rpc.AdminOp{Op: "fsck"}, &rep); err != nil {
		return false, 0, "", err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "%d violations, %d orphan shards", len(rep.Violations), rep.OrphanShards)
	for _, v := range rep.Violations {
		text.WriteString("; " + v)
	}
	return rep.OK, rep.OrphanShards, text.String(), nil
}

func (a *Admin) Chunks(ctx context.Context, n NodeRef, path string) ([]admin.ChunkReport, error) {
	var reps []admin.ChunkReport
	err := a.nodeOp(ctx, n, rpc.AdminOp{Op: "chunk", Chunk: path}, &reps)
	return reps, err
}

func (a *Admin) FSMHash(ctx context.Context, n NodeRef) (uint64, string, error) {
	var h struct {
		Applied uint64 `json:"applied"`
		Hash    string `json:"hash"`
	}
	err := a.call(ctx, n.RPC, http.MethodGet, "/dev/fsm-hash", nil, &h)
	return h.Applied, h.Hash, err
}

func (a *Admin) Goroutines(ctx context.Context, n NodeRef) (string, error) {
	raw, err := a.Caller.Call(ctx, n.RPC, http.MethodGet, "/dev/goroutines", nil)
	return string(raw), err
}

func (a *Admin) Corrupt(ctx context.Context, n NodeRef, key string) error {
	return a.call(ctx, n.RPC, http.MethodPost, "/dev/corrupt?key="+key, nil, nil)
}

// SetFault installs the node's drop rules (nil clears them): /dev/fault, docs/08.
func (a *Admin) SetFault(ctx context.Context, n NodeRef, drop []string) error {
	body, _ := json.Marshal(map[string]any{"drop": drop})
	return a.call(ctx, n.RPC, http.MethodPost, "/dev/fault", body, nil)
}

// Exit asks the node's process to exit (/dev/exit); its service manager restarts it.
func (a *Admin) Exit(ctx context.Context, n NodeRef) error {
	return a.call(ctx, n.RPC, http.MethodPost, "/dev/exit", nil, nil)
}

// AwaitHealth polls /health until the node answers or the timeout passes.
func (a *Admin) AwaitHealth(ctx context.Context, n NodeRef, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := a.call(hctx, n.RPC, http.MethodGet, "/health", nil, nil)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not come back: %w", n.Name, err)
		}
		if err := a.Wait(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}
