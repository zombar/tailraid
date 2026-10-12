package node

import (
	"cmp"
	"context"
	"fmt"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
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

// snapshotCreate begins a manual snapshot on one mirror by name, or on every mirror; a
// mirror that is offline ends it as failed when its host's worker meets it. A manual
// snapshot is permanent: nothing removes it (docs/16 §4, I-183).
func (a *adminOps) snapshotCreate(ctx context.Context, op rpc.AdminOp) (any, error) {
	now := a.clk.Now()
	name := cmp.Or(op.Name, now.UTC().Format("2006-01-02-1504Z"))
	var begun []SnapshotInfo
	nodes := a.node.FSM.Nodes()
	for i := range nodes {
		m := &nodes[i]
		if m.Kind != meta.KindMirror || m.State != meta.Active || (op.Mirror != "" && m.Name != op.Mirror) {
			continue
		}
		cmd := meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: m.Idx, Name: name, Now: now.UnixNano()}}
		if err := a.propose(ctx, cmd); err != nil {
			return begun, fmt.Errorf("mirror %s: %w", m.Name, err)
		}
		begun = append(begun, SnapshotInfo{Mirror: m.Name, Name: name, State: meta.SnapSyncing.String()})
	}
	if len(begun) == 0 {
		return nil, fmt.Errorf("%w: no such mirror %q", meta.ErrNoSuchNode, op.Mirror)
	}
	return begun, nil
}
