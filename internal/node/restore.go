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
		n, err := a.fs.RevertFile(ctx, snapshotPath(op.Mirror, op.Name, p), p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": p, "uploaded": n}, nil
	case "versions_copy":
		if op.Mirror == "" || op.Name == "" || op.As == "" || strings.Contains(op.As, "/") {
			return nil, errors.New("versions --copy needs <mirror>/<name> and --as <name>")
		}
		dst := path.Join(path.Dir(p), op.As)
		rep, err := a.fs.RestoreSnapshot(ctx, snapshotPath(op.Mirror, op.Name, p), dst)
		if err != nil {
			return nil, err
		}
		return restoreReply(dst, rep), nil
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
	return restoreReply(dst, rep), nil
}

// ReplyPaths bounds the skipped and refused paths a restore's reply lists: a restore run
// again over a large tree skips every file, and the client reads at most 4 MiB of reply.
const ReplyPaths = 1000

// restoreReply is a restore's answer: the report with its path lists bounded and the
// whole counts beside it.
func restoreReply(dst string, rep vfs.RestoreReport) map[string]any {
	reply := map[string]any{"path": dst, "skipped_count": len(rep.Skipped), "refused_count": len(rep.Refused)}
	rep.Skipped = rep.Skipped[:min(len(rep.Skipped), ReplyPaths)]
	rep.Refused = rep.Refused[:min(len(rep.Refused), ReplyPaths)]
	reply["report"] = rep
	return reply
}
