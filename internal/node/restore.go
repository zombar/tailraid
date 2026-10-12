package node

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/vfs"
)

// snapshotPath is where path p of snapshot name on mirror lies on the mounts: mirror is
// the row's name (pike/m1) or its directory (pike-m1).
func snapshotPath(mirror, name, p string) string {
	return path.Join("/", vfs.SnapshotDir, strings.ReplaceAll(mirror, "/", "-"), name, p)
}

// VersionsReply is the answer of `versions`: the file's versions newest first (the live
// one first) and the mirrors that were offline.
type VersionsReply struct {
	Path     string              `json:"path"`
	Versions []vfs.Version       `json:"versions"`
	Offline  []vfs.OfflineMirror `json:"offline,omitempty"`
}

// restoreOp answers versions, versions_revert, versions_copy and snapshot_restore on the
// node asked, through its vfs as the gateways' writes (docs/12 §11, docs/16 §6).
func (a *adminOps) restoreOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	p := path.Join("/", op.Path)
	switch op.Op {
	case "versions":
		vs, off, err := a.fs.Versions(ctx, p)
		if err != nil {
			return nil, err
		}
		return VersionsReply{Path: p, Versions: vs, Offline: off}, nil
	case "versions_revert":
		if op.Mirror == "" || op.Name == "" {
			return nil, errors.New("versions --revert needs <mirror>/<name>")
		}
		if err := a.fs.RevertFile(ctx, snapshotPath(op.Mirror, op.Name, p), p); err != nil {
			return nil, err
		}
		return map[string]string{"path": p}, nil
	case "versions_copy":
		if op.Mirror == "" || op.Name == "" || op.As == "" || strings.Contains(op.As, "/") {
			return nil, errors.New("versions --copy needs <mirror>/<name> and --as <name>")
		}
		dst := path.Join(path.Dir(p), op.As)
		rep, err := a.fs.RestoreSnapshot(ctx, snapshotPath(op.Mirror, op.Name, p), dst)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": dst, "report": rep}, nil
	}
	// snapshot_restore: to the same path, or under To, named As
	if op.Mirror == "" || op.Name == "" {
		return nil, errors.New("snapshot restore needs <mirror>/<name>")
	}
	dst := p
	if op.To != "" {
		dst = path.Join("/", op.To, path.Base(p))
	}
	if op.As != "" {
		if strings.Contains(op.As, "/") {
			return nil, errors.New("--as is a name, not a path")
		}
		dst = path.Join(path.Dir(dst), op.As)
	}
	rep, err := a.fs.RestoreSnapshot(ctx, snapshotPath(op.Mirror, op.Name, p), dst)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": dst, "report": rep}, nil
}
