package node

import (
	"cmp"
	"context"
	"fmt"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/vfs"
	"github.com/zombar/tailraid/internal/webui"
)

// SnapshotInfo is one catalogue row as `snapshot ls` and the web UI show it.
type SnapshotInfo struct {
	ID       uint32 `json:"id"`
	Mirror   string `json:"mirror"`
	Name     string `json:"name"`
	Index    uint64 `json:"index"`
	At       int64  `json:"at"`
	Auto     bool   `json:"auto"`
	State    string `json:"state"`
	Files    uint64 `json:"files"`
	Bytes    uint64 `json:"bytes"`
	NewBytes uint64 `json:"new_bytes"`
	Missing  uint32 `json:"missing,omitempty"`
}

// snapshotList is the catalogue newest first, without Deleting rows (they are gone for
// the user, I-183), of one mirror by name or every one.
func snapshotList(nodes []meta.Node, snaps []meta.Snapshot, mirrorName string) []SnapshotInfo {
	names := map[meta.NodeIdx]string{}
	for i := range nodes {
		if nodes[i].Kind == meta.KindMirror {
			names[nodes[i].Idx] = nodes[i].Name
		}
	}
	out := []SnapshotInfo{}
	for i := len(snaps) - 1; i >= 0; i-- {
		sn := &snaps[i]
		if sn.State == meta.SnapDeleting || (mirrorName != "" && names[sn.Mirror] != mirrorName) {
			continue
		}
		out = append(out, SnapshotInfo{
			ID: uint32(sn.ID), Mirror: names[sn.Mirror], Name: sn.Name, Index: sn.Index, At: sn.At, Auto: sn.Auto,
			State: sn.State.String(), Files: sn.Files, Bytes: sn.Bytes, NewBytes: sn.NewBytes, Missing: sn.Missing,
		})
	}
	return out
}

// snapshotCreate begins a manual snapshot on one mirror by name, or on every online
// mirror. A named mirror that is offline is refused with vfs.ErrMirrorOffline, one already
// syncing with meta.ErrExist; with no name, the offline and syncing mirrors are passed
// over and none left is ErrMirrorOffline. A manual snapshot is permanent: nothing removes
// it (docs/16 §4, I-183).
func (a *adminOps) snapshotCreate(ctx context.Context, op rpc.AdminOp) (any, error) {
	now := a.clk.Now()
	name := cmp.Or(op.Name, now.UTC().Format("2006-01-02-1504Z"))
	syncing := map[meta.NodeIdx]bool{}
	for _, sn := range a.node.FSM.Snapshots() {
		if sn.State == meta.SnapSyncing {
			syncing[sn.Mirror] = true
		}
	}
	online := func(m meta.NodeIdx) bool { // a mirror open here is online now; another host's by the probes (docs/16 §1)
		_, here := a.mirrors.get(m)
		return here || a.prober.MirrorOnline(m)
	}
	var begun []SnapshotInfo
	named := false
	nodes := a.node.FSM.Nodes()
	for i := range nodes {
		m := &nodes[i]
		if m.Kind != meta.KindMirror || m.State != meta.Active || (op.Mirror != "" && m.Name != op.Mirror) {
			continue
		}
		named = op.Mirror != ""
		switch {
		case !online(m.Idx) && named:
			return nil, fmt.Errorf("mirror %s: %w: attach it first", m.Name, vfs.ErrMirrorOffline)
		case syncing[m.Idx] && named:
			return nil, fmt.Errorf("mirror %s is syncing a snapshot already: %w", m.Name, meta.ErrExist)
		case !online(m.Idx), syncing[m.Idx]:
			continue
		}
		cmd := meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: m.Idx, Name: name, Now: now.UnixNano()}}
		if err := a.propose(ctx, cmd); err != nil {
			return begun, fmt.Errorf("mirror %s: %w", m.Name, err)
		}
		begun = append(begun, SnapshotInfo{Mirror: m.Name, Name: name, State: meta.SnapSyncing.String()})
	}
	switch {
	case len(begun) > 0:
		return begun, nil
	case op.Mirror != "" && !named:
		return nil, fmt.Errorf("%w: no such mirror %q", meta.ErrNoSuchNode, op.Mirror)
	case op.Mirror == "" && !named && !hasMirror(nodes):
		return nil, fmt.Errorf("%w: the cluster has no mirror (tailraid mirror add)", meta.ErrNoSuchNode)
	}
	return nil, fmt.Errorf("%w: no mirror is online and idle", vfs.ErrMirrorOffline)
}

func hasMirror(nodes []meta.Node) bool {
	for i := range nodes {
		if nodes[i].Kind == meta.KindMirror && nodes[i].State == meta.Active {
			return true
		}
	}
	return false
}

// webSnapshots is the web UI's snapshot service (docs/13 §3): the cluster's mirrors from
// the probes, the schedule, and a manual snapshot after this gateway's dirty files land.
type webSnapshots struct {
	ops   *adminOps
	flush func(ctx context.Context) error
}

var _ webui.SnapshotService = webSnapshots{}

func (w webSnapshots) Mirrors() []admin.MirrorReport {
	snaps := w.ops.node.FSM.Snapshots()
	local, _ := w.ops.mirrors.report(w.ops.clk.Now(), snaps)
	return clusterMirrors(w.ops.node.FSM.Nodes(), snaps, w.ops.prober, w.ops.self, local)
}

func (w webSnapshots) Take(ctx context.Context, name, mirror string) (any, error) {
	if err := w.flush(ctx); err != nil { // the user's own last writes are in it (I-187)
		return nil, fmt.Errorf("flush before the snapshot: %w", err)
	}
	return w.ops.snapshotCreate(ctx, rpc.AdminOp{Op: "snapshot_create", Name: name, Mirror: mirror})
}

// StartRestore starts a restore job as the admin op does, for the session's user, whose
// role where the copy lands the web handler checked (docs/16 §6).
func (w webSnapshots) StartRestore(ctx context.Context, actor meta.UserIdx, mirror, snapshot, src, dst string) (rpc.RestoreStarted, error) {
	return w.ops.startRestore(ctx, rpc.AdminOp{Op: "snapshot_restore", Actor: actor, Mirror: mirror, Name: snapshot}, src, dst)
}

// CancelRestore cancels a restore job as the admin op does, on the job's host.
func (w webSnapshots) CancelRestore(ctx context.Context, actor meta.UserIdx, job string) error {
	_, err := w.ops.cancelRestore(ctx, rpc.AdminOp{Op: "restore_cancel", Actor: actor, Job: job})
	return err
}

func (w webSnapshots) Schedule() webui.Schedule {
	p := w.ops.mirrorPol
	return webui.Schedule{Every: p.Every, Hourly: p.Hourly, Daily: p.Daily, Weekly: p.Weekly, Monthly: p.Monthly, StaleAfter: p.StaleAfter}
}
