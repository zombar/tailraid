package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/restore"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/vfs"
)

// VersionsReply is the answer of `versions`: the file's versions newest first (the live
// one first) and the mirrors that were offline.
type VersionsReply struct {
	Path     string              `json:"path"`
	Versions []vfs.Version       `json:"versions"`
	Offline  []vfs.OfflineMirror `json:"offline,omitempty"`
}

// restoreOp answers versions, versions_revert, versions_copy and snapshot_restore on the
// node asked, through its vfs as the gateways' writes (docs/12 §11, docs/16 §6), as the
// user who signed the op (docs/21 §4, I-348).
func (a *adminOps) restoreOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	p := path.Join("/", op.Path)
	fs := a.actorFS(op)
	switch op.Op {
	case "versions":
		vs, off, err := fs.Versions(ctx, p)
		if err != nil {
			return nil, err
		}
		return VersionsReply{Path: p, Versions: vs, Offline: off}, nil
	case "versions_revert":
		if op.Mirror == "" || op.Name == "" {
			return nil, errors.New("versions --revert needs <mirror>/<name>")
		}
		n, err := fs.RevertFile(ctx, restore.SnapshotPath(op.Mirror, op.Name, p), p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": p, "uploaded": n}, nil
	case "versions_copy":
		if op.Mirror == "" || op.Name == "" || op.As == "" || strings.Contains(op.As, "/") {
			return nil, errors.New("versions --copy needs <mirror>/<name> and --as <name>")
		}
		dst := path.Join(path.Dir(p), op.As)
		rep, err := fs.RestoreSnapshot(ctx, restore.SnapshotPath(op.Mirror, op.Name, p), dst)
		if err != nil {
			return nil, err
		}
		return restoreReply(dst, rep), nil
	}
	if op.Op == "restore_cancel" {
		return a.cancelRestore(ctx, op)
	}
	// snapshot_restore: a job on the mirror's host, to the same path, or under To, named As
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
	return a.startRestore(ctx, op, p, dst)
}

// startRestore records a tree restore as a job on the mirror's host and answers once the
// host has (docs/16 §6, I-363): here when this node hosts the mirror, else forwarded. The
// caller's role where the copy lands was checked before the op ran (authorize); the job
// checks it again when it runs.
func (a *adminOps) startRestore(ctx context.Context, op rpc.AdminOp, src, dst string) (rpc.RestoreStarted, error) {
	if a.restores == nil {
		return rpc.RestoreStarted{}, errors.New("snapshot restore: this node runs no restore jobs")
	}
	row, ok := a.mirrorRow(op.Mirror)
	if !ok {
		return rpc.RestoreStarted{}, fmt.Errorf("snapshot restore: no mirror %q: %w", op.Mirror, meta.ErrNotExist)
	}
	var sn meta.Snapshot
	for _, s := range a.restores.Store().Snapshots() {
		if s.Mirror == row.Idx && s.Name == op.Name {
			sn = s
		}
	}
	if sn.ID == 0 {
		return rpc.RestoreStarted{}, fmt.Errorf("snapshot restore: no snapshot %s on %s: %w", op.Name, row.Name, meta.ErrNotExist)
	}
	// a file or link lands only where nothing is (a folder merges): refused here, so a job
	// that finds its one file in place has only to be a resumed one (docs/16 §6)
	fs := a.actorFS(op)
	if fi, err := fs.Lstat(restore.SnapshotPath(row.Name, sn.Name, src)); err == nil && !fi.IsDir() {
		if _, err := fs.Lstat(dst); err == nil {
			return rpc.RestoreStarted{}, fmt.Errorf("snapshot restore %s: %w", dst, os.ErrExist)
		}
	}
	req := rpc.RestoreStart{Mirror: row.Idx, Snapshot: sn.ID, Path: src, Dst: dst, Actor: op.Actor}
	if self, ok := a.restores.Self(); ok && self == row.Host {
		return a.restores.Start(ctx, req)
	}
	addr, _, ok := a.node.RPCAddrOf(row.Host)
	if !ok {
		return rpc.RestoreStarted{}, fmt.Errorf("snapshot restore: the host of %s has no address", row.Name)
	}
	started, err := a.client.StartRestore(ctx, addr, req)
	if errors.Is(err, rpc.ErrNoRestoreJobs) {
		host, _ := a.restores.Store().Node(row.Host)
		return rpc.RestoreStarted{}, fmt.Errorf("%s runs tailraid %s, which has no restore jobs; update it", host.Name, cmp.Or(host.Version, "of an older version"))
	}
	return started, err
}

// cancelRestore asks the job's host to cancel it for the op's user (docs/16 §6, I-368).
func (a *adminOps) cancelRestore(ctx context.Context, op rpc.AdminOp) (any, error) {
	host, ok := restore.HostOf(op.Job)
	if !ok || a.restores == nil {
		return nil, fmt.Errorf("%w: %s", rpc.ErrNoRestore, op.Job)
	}
	if self, ok := a.restores.Self(); ok && self == host {
		return map[string]string{"cancelled": op.Job}, a.restores.Cancel(ctx, op.Job, op.Actor)
	}
	addr, _, ok := a.node.RPCAddrOf(host)
	if !ok {
		return nil, fmt.Errorf("%w: %s", rpc.ErrNoRestore, op.Job)
	}
	return map[string]string{"cancelled": op.Job}, a.client.CancelRestore(ctx, addr, op.Job, op.Actor)
}

// mirrorRow is the mirror named by its row name (pike/m1) or its directory (pike-m1).
func (a *adminOps) mirrorRow(name string) (meta.Node, bool) {
	nodes := a.restores.Store().Nodes()
	for i := range nodes {
		if n := &nodes[i]; n.Kind == meta.KindMirror && n.State != meta.Removed && (n.Name == name || strings.ReplaceAll(n.Name, "/", "-") == name) {
			return *n, true
		}
	}
	return meta.Node{}, false
}

// actorFS is the vfs an admin op writes through: the signer's view once users exist, so
// the FSM checks the signer's role on every proposal; before the first user an op is
// unsigned and the node's own view writes, as the gateways did then.
func (a *adminOps) actorFS(op rpc.AdminOp) *vfs.FS {
	if op.Actor == 0 {
		return a.fs
	}
	return a.fs.As(op.Actor)
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
