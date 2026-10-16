package soak_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/restore"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// The harness's mirror (phase 19, docs/16): a mirror store in a temp dir on node 1, read
// by every node's vfs through testharness.Mirrors. A snapshot is synced in the call that
// begins it, as the host's worker would; plugging the mirror back in takes the catch-up
// its host's next tick would (an automatic snapshot), since the harness runs no tick.

const harnessMirrorHost = 1

// harnessMirror is the mirror's state on the harness environment.
type harnessMirror struct {
	src  *testharness.Mirrors
	idx  meta.NodeIdx
	name string
	path string
	host int
}

func (e *harnessEnv) AddMirror(ctx context.Context) (string, soak.NodeRef, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	host := e.c.Nodes[harnessMirrorHost]
	ref := soak.NodeRef{Name: fmt.Sprintf("n%d", host.I), Idx: host.Idx(), RPC: host.RPCAddr()}
	if e.mirror != nil {
		return e.mirror.name, ref, nil
	}
	id := "harness-mirror-1"
	name := ref.Name + "/m1"
	if err := e.c.Leader().Node.MemberOp(ctx, rpc.MemberOp{Op: "add_mirror", NodeIdx: host.Idx(), DiskID: id, Name: name}); err != nil {
		return "", soak.NodeRef{}, err
	}
	e.c.Settle()
	var idx meta.NodeIdx
	nodes := e.c.Leader().Node.FSM.Nodes()
	for i := range nodes {
		if nodes[i].ID == id {
			idx = nodes[i].Idx
		}
	}
	dir := filepath.Join(e.t.TempDir(), "mirror")
	cid := fmt.Sprintf("%x", e.c.CID)
	if err := mirror.Create(sysfs.OS{}, dir, mirror.Marker{Format: mirror.Format, ClusterID: cid, MirrorID: id}); err != nil {
		return "", soak.NodeRef{}, err
	}
	mm, err := mirror.Open(sysfs.OS{}, dir, cid)
	if err != nil {
		return "", soak.NodeRef{}, err
	}
	src := testharness.NewMirrors()
	src.Add(idx, mm)
	e.mirror = &harnessMirror{src: src, idx: idx, name: name, path: dir, host: host.I}
	return name, ref, nil
}

func (e *harnessEnv) SetMirrorOnline(ctx context.Context, _ string, online bool) error {
	e.mu.RLock()
	hm := e.mirror
	e.mu.RUnlock()
	if hm == nil {
		return errors.New("no mirror")
	}
	hm.src.SetOnline(hm.idx, online)
	if online { // the host's first tick after the return: the catch-up
		now := e.c.Clk.Now().UTC()
		_, err := e.takeSnapshot(ctx, now.Format("2006-01-02-1504Z"), true)
		if errors.Is(err, meta.ErrExist) { // plugged back within the minute of an earlier one
			_, err = e.takeSnapshot(ctx, now.Format("2006-01-02-150405Z"), true)
		}
		return err
	}
	return nil
}

// fsOf is node n's vfs with the harness's mirrors.
func (e *harnessEnv) fsOf(n soak.NodeRef) *vfs.FS {
	nd := e.node(n)
	if e.mirror != nil {
		nd.FS.SetMirrors(e.mirror.src)
	}
	return nd.FS
}

func (e *harnessEnv) SnapshotOp(ctx context.Context, n soak.NodeRef, op rpc.AdminOp) (json.RawMessage, error) {
	var out any
	var err error
	switch op.Op {
	case "snapshot_ls":
		out = e.snapshotList(op.Mirror)
	case "snapshot_create":
		var sn meta.Snapshot
		if sn, err = e.takeSnapshot(ctx, op.Name, false); err == nil {
			out = []soak.SnapshotInfo{{Mirror: e.mirror.name, Name: sn.Name, State: sn.State.String()}}
		}
	default:
		actor := e.actor(e.node(n))
		e.mu.RLock()
		fs := e.fsOf(n)
		e.mu.RUnlock()
		if actor != 0 { // the signer's view, as node.adminOps' restoreOp
			fs = fs.As(actor)
		}
		p := path.Join("/", op.Path)
		src := path.Join("/", vfs.SnapshotDir, strings.ReplaceAll(op.Mirror, "/", "-"), op.Name, p)
		switch op.Op {
		case "versions":
			vs, off, verr := fs.Versions(ctx, p)
			out, err = map[string]any{"path": p, "versions": vs, "offline": off}, verr
		case "versions_revert":
			n, rerr := fs.RevertFile(ctx, src, p)
			out, err = map[string]any{"path": p, "uploaded": n}, rerr
		case "snapshot_restore", "restore_cancel": // a job on the mirror's host (docs/16 §6)
			op.Actor = actor
			out, err = e.restoreOp(ctx, op)
		default:
			return nil, fmt.Errorf("harness: no snapshot op %q", op.Op)
		}
		e.c.Settle()
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// snapshotList is `snapshot ls` over the leader's catalogue, newest first.
func (e *harnessEnv) snapshotList(mirrorName string) []soak.SnapshotInfo {
	fsm := e.c.Leader().Node.FSM
	names := map[meta.NodeIdx]string{}
	nodes := fsm.Nodes()
	for i := range nodes {
		if n := &nodes[i]; n.Kind == meta.KindMirror {
			names[n.Idx] = n.Name
		}
	}
	snaps := fsm.Snapshots()
	out := []soak.SnapshotInfo{}
	for i := len(snaps) - 1; i >= 0; i-- {
		sn := &snaps[i]
		if sn.State == meta.SnapDeleting || (mirrorName != "" && names[sn.Mirror] != mirrorName) {
			continue
		}
		out = append(out, soak.SnapshotInfo{
			ID: uint32(sn.ID), Mirror: names[sn.Mirror], Name: sn.Name, Index: sn.Index, At: sn.At, Auto: sn.Auto,
			State: sn.State.String(), Files: sn.Files, Bytes: sn.Bytes, NewBytes: sn.NewBytes, Missing: sn.Missing,
		})
	}
	return out
}

// takeSnapshot begins a snapshot on the mirror through the leader and syncs it from the
// clone the host's FSM hands over at its index, as the host's worker does (docs/16 §3).
func (e *harnessEnv) takeSnapshot(ctx context.Context, name string, auto bool) (meta.Snapshot, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	hm := e.mirror
	if hm == nil || !hm.src.Online(hm.idx) {
		return meta.Snapshot{}, errors.New("the mirror is offline")
	}
	host := e.c.Nodes[hm.host]
	clones := make(chan *meta.State, 1)
	host.Node.FSM.SetSnapshotHook(func(_ meta.Snapshot, h meta.NodeIdx, _ string, clone func() *meta.State) {
		if h == host.Idx() {
			clones <- clone()
		}
	})
	defer host.Node.FSM.SetSnapshotHook(nil)
	res, index, err := e.c.Leader().Node.Propose(ctx, meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: hm.idx, Name: name, Auto: auto, Now: e.c.Clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return meta.Snapshot{}, err
	}
	if err := host.Node.FSM.WaitApplied(ctx, index); err != nil {
		return meta.Snapshot{}, err
	}
	clone := <-clones
	sn, _ := host.Node.FSM.SnapshotRow(res.SnapID)
	man, err := hm.src.Mirror(hm.idx).WriteSnapshot(ctx, mirror.StateSource(clone, host.Reader.ReadChunk), uint64(meta.RootIno), mirror.Manifest{
		ID: uint32(sn.ID), Name: sn.Name, Index: sn.Index, At: sn.At, Auto: sn.Auto, ChunkSize: clone.Vol.ChunkSize,
	})
	end := meta.SnapshotEnd{ID: sn.ID, Now: e.c.Clk.Now().UnixNano()}
	if err == nil {
		end = meta.SnapshotEnd{ID: sn.ID, OK: true, Root: meta.Hash(man.Root), Files: man.Files, Bytes: man.Bytes, NewBytes: man.NewBytes, Missing: uint32(len(man.Missing)), Now: end.Now}
	}
	if res, _, perr := e.c.Leader().Node.Propose(ctx, meta.Command{Op: meta.OpSnapshotEnd, SnapshotEnd: &end}); perr != nil || res.Err != nil {
		return meta.Snapshot{}, errors.Join(err, perr, res.Err)
	}
	e.c.Settle()
	sn, _ = e.c.Leader().Node.FSM.SnapshotRow(sn.ID)
	return sn, err
}

// mirrorReports is the host's mirrors as its status reports them.
func (e *harnessEnv) mirrorReports(i int) []admin.MirrorReport {
	hm := e.mirror
	if hm == nil || hm.host != i {
		return nil
	}
	r := admin.MirrorReport{Name: hm.name, Path: hm.path, Online: hm.src.Online(hm.idx)}
	if r.Online {
		r.Used = hm.src.Mirror(hm.idx).Used()
	} else {
		r.Reason = "not attached"
	}
	return []admin.MirrorReport{r}
}

// harnessJobs is the mirror host's restore jobs: the node's own runner (internal/restore)
// over its current handles, its file in a directory that outlives a restart, so a
// restarted host resumes what it recorded (I-364).
type harnessJobs struct {
	dir  string
	fs   *vfs.FS // the handles the runner was built over
	jobs *restore.Jobs
	stop context.CancelFunc
	done chan struct{}
}

// restoreJobs is the mirror host's runner, started over the host's current handles (a
// restart replaced them: the old runner is stopped and a new one resumes from the file);
// nil while there is no mirror or its host is down. e.mu held.
func (e *harnessEnv) restoreJobs() *restore.Jobs {
	hm := e.mirror
	if hm == nil || !e.c.Alive(hm.host) {
		e.stopRestoreJobs()
		return nil
	}
	nd := e.c.Nodes[hm.host]
	nd.FS.SetMirrors(hm.src)
	if e.jobs != nil && e.jobs.fs == nd.FS {
		return e.jobs.jobs
	}
	e.stopRestoreJobs()
	if e.jobsDir == "" {
		e.jobsDir = e.t.TempDir()
		e.t.Cleanup(func() { e.mu.Lock(); e.stopRestoreJobs(); e.mu.Unlock() })
	}
	idx, src := hm.idx, hm.src
	jobs := restore.New(e.jobsDir, nd.FS, nd.Node.FSM, func() (meta.NodeIdx, bool) { return nd.Idx(), true },
		func(m meta.NodeIdx) bool { return m == idx && src.Online(m) }, e.c.Clk, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	hj := &harnessJobs{dir: e.jobsDir, fs: nd.FS, jobs: jobs, stop: cancel, done: make(chan struct{})}
	go func() { defer close(hj.done); jobs.Run(ctx) }()
	e.jobs = hj
	return jobs
}

// stopRestoreJobs stops the runner, as the host's process ending does. e.mu held.
func (e *harnessEnv) stopRestoreJobs() {
	if e.jobs == nil {
		return
	}
	e.jobs.stop()
	<-e.jobs.done
	e.jobs = nil
}

// restoreList is every host's restore jobs as every node's status lists them (I-365).
func (e *harnessEnv) restoreList() []rpc.RestoreJob {
	e.mu.Lock()
	jobs := e.restoreJobs()
	e.mu.Unlock()
	if jobs == nil {
		return nil
	}
	out := jobs.List()
	restore.Sort(out)
	return out
}

// restoreOp starts or cancels a restore job as node.adminOps does: the mirror's host
// records the job and answers; a cancel is the host's, for the job's starter or an admin.
func (e *harnessEnv) restoreOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	jobs := e.restoreJobs()
	if jobs == nil {
		return nil, errors.New("harness: no mirror host for restore jobs")
	}
	if op.Op == "restore_cancel" {
		return map[string]string{"cancelled": op.Job}, jobs.Cancel(ctx, op.Job, op.Actor)
	}
	st := jobs.Store()
	var row meta.Node
	nodes := st.Nodes()
	for i := range nodes {
		if n := &nodes[i]; n.Kind == meta.KindMirror && (n.Name == op.Mirror || strings.ReplaceAll(n.Name, "/", "-") == op.Mirror) {
			row = *n
		}
	}
	var sn meta.Snapshot
	for _, s := range st.Snapshots() {
		if s.Mirror == row.Idx && s.Name == op.Name {
			sn = s
		}
	}
	if sn.ID == 0 {
		return nil, fmt.Errorf("snapshot restore: no snapshot %s on %s: %w", op.Name, op.Mirror, meta.ErrNotExist)
	}
	src := path.Join("/", op.Path)
	dst := src
	if op.To != "" {
		dst = path.Join("/", op.To, path.Base(src))
	}
	if op.As != "" {
		dst = path.Join(path.Dir(dst), op.As)
	}
	return jobs.Start(ctx, rpc.RestoreStart{Mirror: row.Idx, Snapshot: sn.ID, Path: src, Dst: dst, Actor: op.Actor})
}
