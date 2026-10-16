package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/restore"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// gateSrc is the fabric's mirrors with a gate: a read of block waits until release
// closes (blocked closes once one does), and onRead is told every object read.
type gateSrc struct {
	*testharness.Mirrors
	mu      sync.Mutex
	block   meta.Hash
	release chan struct{}
	blocked chan struct{}
	once    sync.Once
	onRead  func()
}

func (g *gateSrc) hold(h meta.Hash) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.block, g.release, g.blocked, g.once = h, make(chan struct{}), make(chan struct{}), sync.Once{}
}

func (g *gateSrc) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	}
}

func (g *gateSrc) Object(ctx context.Context, m meta.NodeIdx, h meta.Hash) ([]byte, error) {
	g.mu.Lock()
	on, block, release, blocked := g.onRead, g.block != meta.HashZero && h == g.block, g.release, g.blocked
	g.mu.Unlock()
	if on != nil {
		on()
	}
	if block {
		g.once.Do(func() { close(blocked) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Mirrors.Object(ctx, m, h)
}

// jobRig is a host with snapshot s1 of /docs on its mirror pike/m1, the live files since
// rewritten and the snapshot's chunks collected, so a restore reads every chunk from the
// mirror, and its restore jobs.
type jobRig struct {
	t    *testing.T
	f    *testharness.Fabric
	fs   *vfs.FS
	src  *testharness.Mirrors
	gate *gateSrc
	m    meta.NodeIdx
	sn   meta.Snapshot
	dir  string
	want map[string][]byte
	big  meta.Hash // the first chunk of /docs/a.bin
	jobs *restore.Jobs
	stop func()
}

func newJobRig(t *testing.T) *jobRig {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	r := &jobRig{t: t, f: f, fs: f.FS(), src: testharness.NewMirrors(), dir: t.TempDir(), want: map[string][]byte{}}
	r.fs.SetFlusher(f.Flusher())
	cs := int(f.FSM.Volume().ChunkSize)
	for name, data := range map[string][]byte{
		"/docs/a.bin": testharness.Pattern(3*cs+cs/2, 1), "/docs/b.txt": []byte("small"), "/docs/sub/c.bin": testharness.Pattern(cs+100, 2),
	} {
		if err := r.fs.MkdirAll(path.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		fabricWrite(t, r.fs, name, data)
		r.want[name] = data
	}
	if err := f.Flusher().FlushAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	in, err := f.FSM.Resolve([]string{"docs", "a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	r.big = in.Chunks[0]
	r.m = f.AddMirror(r.src, "pike/m1")
	r.sn = f.TakeSnapshot(r.src, r.m, "s1")
	r.gate = &gateSrc{Mirrors: r.src}
	r.fs.SetMirrors(r.gate)
	// the live files are rewritten, so their inodes stay (a user's role on them reads the
	// snapshot's, I-348) and their old chunks are collected: a restore reads the mirror
	for name, data := range r.want {
		fabricWrite(t, r.fs, name, bytes.Repeat([]byte("x"), len(data)))
	}
	if err := f.Flusher().FlushAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.MustApply(meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: 1 << 62}})
	if _, ok := f.FSM.GetChunk(r.big); ok {
		t.Fatal("the snapshot's chunk is still in the pool")
	}
	r.run()
	return r
}

// run starts this host's restore jobs from its file, as a node start does.
func (r *jobRig) run() {
	r.jobs = restore.New(r.dir, r.fs, r.f.FSM, func() (meta.NodeIdx, bool) { return r.f.SelfIdx, true }, r.src.Online, r.f.Clk, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.jobs.Run(ctx); close(done) }()
	var once sync.Once
	r.stop = func() { once.Do(func() { cancel(); <-done }) }
	r.t.Cleanup(r.stop)
}

// restart stops the host and starts it again.
func (r *jobRig) restart() {
	r.stop()
	r.run()
}

func (r *jobRig) start(actor meta.UserIdx, dst string) string {
	r.t.Helper()
	started, err := r.jobs.Start(context.Background(), rpc.RestoreStart{Mirror: r.m, Snapshot: r.sn.ID, Path: "/docs", Dst: dst, Actor: actor})
	if err != nil {
		r.t.Fatal(err)
	}
	return started.Job
}

func (r *jobRig) job(id string) rpc.RestoreJob {
	jobs := r.jobs.List()
	for i := range jobs {
		if jobs[i].ID == id {
			return jobs[i]
		}
	}
	return rpc.RestoreJob{}
}

// await waits until job id is in state, advancing the fake clock so a waiting or
// retrying job's timer fires.
func (r *jobRig) await(id, state string) rpc.RestoreJob {
	r.t.Helper()
	testharness.EventuallyWithin(r.t, 20*time.Second, func() bool {
		if r.job(id).State == state {
			return true
		}
		r.f.Clk.Advance(time.Second)
		return false
	}, func() string { return fmt.Sprintf("job %s is %+v, want %s", id, r.job(id), state) })
	return r.job(id)
}

// matches checks the restored tree under dst holds the snapshot's bytes and no temporary file.
func (r *jobRig) matches(dst string) {
	r.t.Helper()
	for name, data := range r.want {
		if got := fabricRead(r.t, r.fs, dst+strings.TrimPrefix(name, "/docs")); !bytes.Equal(got, data) {
			r.t.Fatalf("%s differs at %d", name, testharness.FirstDiff(got, data))
		}
	}
	noTemp(r.t, r.fs, dst)
}

func noTemp(t *testing.T, fs *vfs.FS, dir string) {
	t.Helper()
	fis, err := fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, fi := range fis {
		if strings.HasPrefix(fi.Name(), ".tailraid-restore-") {
			t.Fatalf("a temporary file is left: %s", path.Join(dir, fi.Name()))
		}
		if fi.IsDir() {
			noTemp(t, fs, path.Join(dir, fi.Name()))
		}
	}
}

// A tree restore started through a node that is not the mirror's host is recorded on the
// host, whose answer comes while the job still waits on its first chunks; the request is
// over, and the job goes on to the snapshot's bytes (docs/16 §6).
//
// Invariant: I-363
func TestRestoreJob_OutlivesRequest(t *testing.T) {
	r := newJobRig(t)
	r.gate.hold(r.big)
	cid := [16]byte{9}
	srv, err := rpc.NewServer(rpc.Options{NodeID: "host", ClusterID: cid, Applier: restoreApplier{}, Auth: rpc.AllowAll{}, Members: func() []meta.Node { return nil }, Restores: r.jobs})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	cl := rpc.NewClient(cid, nil)
	rctx, cancel := context.WithCancel(context.Background())
	started, err := cl.StartRestore(rctx, hs.Listener.Addr().String(), rpc.RestoreStart{Mirror: r.m, Snapshot: r.sn.ID, Path: "/docs", Dst: "/back", Actor: 0})
	cancel() // the asker is gone: closed, refreshed, interrupted
	if err != nil {
		t.Fatal(err)
	}
	<-r.gate.blocked // the job runs on its own
	if st := r.job(started.Job).State; st != rpc.RestoreRunning {
		t.Fatalf("the job is %s while its chunk is read", st)
	}
	if h, err := cl.Health(context.Background(), hs.Listener.Addr().String()); err != nil || h.Restores != 1 {
		t.Fatalf("/health counts %d restore jobs (%v), want 1", h.Restores, err)
	}
	r.gate.open()
	r.await(started.Job, rpc.RestoreDone)
	r.matches("/back")
	if _, err := cl.StartRestore(context.Background(), hs.Listener.Addr().String(), rpc.RestoreStart{Mirror: r.m + 99, Snapshot: r.sn.ID, Path: "/docs", Dst: "/x"}); !errors.Is(err, rpc.ErrBadRequest) {
		t.Fatalf("a start on a node that does not host the mirror: %v", err)
	}
}

type restoreApplier struct{}

func (restoreApplier) Propose(context.Context, meta.Command) (meta.Result, uint64, error) {
	return meta.Result{}, 0, nil
}
func (restoreApplier) AppliedIndex() uint64  { return 0 }
func (restoreApplier) IsLeader() bool        { return true }
func (restoreApplier) LeaderRPCAddr() string { return "" }
func (restoreApplier) Ready() bool           { return true }

// A job resumes: the host stopped while it copied starts again and finishes it, with the
// snapshot's bytes and no temporary file; a job whose mirror goes offline waits and goes
// on once it is back; and while a job is unfinished the host's thinning leaves the
// snapshot it reads.
//
// Invariant: I-364
func TestRestoreJob_ResumesAfterRestartAndOfflineMirror(t *testing.T) {
	r := newJobRig(t)
	r.gate.hold(r.big)
	id := r.start(0, "/back")
	<-r.gate.blocked
	if !r.jobs.Held()[r.sn.ID] {
		t.Fatal("the snapshot a running job reads is not held from thinning")
	}
	ms := &mirrorSet{held: r.jobs.Held}
	if got := ms.unheld([]meta.SnapID{r.sn.ID, r.sn.ID + 1}); len(got) != 1 || got[0] != r.sn.ID+1 {
		t.Fatalf("thinning would delete %v", got)
	}
	r.restart() // the host stops mid-file and comes back
	r.gate.open()
	job := r.await(id, rpc.RestoreDone)
	if job.Resumed != 1 {
		t.Fatalf("resumed %d times, want 1", job.Resumed)
	}
	r.matches("/back")
	if r.jobs.Held()[r.sn.ID] {
		t.Fatal("a finished job still holds its snapshot")
	}

	r.src.SetOnline(r.m, false)
	id = r.start(0, "/again")
	r.await(id, rpc.RestoreWaiting)
	r.src.SetOnline(r.m, true)
	r.await(id, rpc.RestoreDone)
	r.matches("/again")
}

// A job's totals are counted before its first chunk is read, its done counts never fall,
// across a restart of the host included, and end at the totals; every node's status lists
// the same jobs of every live host in the same order.
//
// Invariant: I-365
func TestRestoreJob_ProgressMonotone_SameOnEveryNode(t *testing.T) {
	r := newJobRig(t)
	var mu sync.Mutex
	var seen []rpc.RestoreJob
	id := ""
	r.gate.onRead = func() {
		mu.Lock()
		defer mu.Unlock()
		if id != "" {
			seen = append(seen, r.job(id))
		}
	}
	r.gate.hold(r.big)
	mu.Lock()
	id = r.start(0, "/back")
	mu.Unlock()
	<-r.gate.blocked
	r.restart()
	r.gate.open()
	end := r.await(id, rpc.RestoreDone)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no chunk was read")
	}
	var files, done int64
	for _, j := range seen {
		if !j.Counted || j.FilesTotal != int64(len(r.want)) {
			t.Fatalf("a chunk read before the totals: %+v", j)
		}
		if j.FilesDone < files || j.BytesDone < done {
			t.Fatalf("done fell from %d files %d bytes to %d and %d", files, done, j.FilesDone, j.BytesDone)
		}
		files, done = j.FilesDone, j.BytesDone
	}
	if end.FilesDone != end.FilesTotal || end.BytesDone != end.BytesTotal || end.BytesTotal == 0 {
		t.Fatalf("the finished job: %+v", end)
	}

	// three nodes build status from the same probes: the same list, the same order
	nodes := []meta.Node{{Idx: 1, Kind: meta.KindDevice}, {Idx: 2, Kind: meta.KindDevice}, {Idx: 3, Kind: meta.KindDevice}}
	byHost := map[meta.NodeIdx][]rpc.RestoreJob{
		1: {{ID: "1-1", Started: 30}, {ID: "1-2", Started: 10}},
		2: {},
		3: {{ID: "3-1", Started: 20}, {ID: "3-2", Started: 30}},
	}
	probes := map[meta.NodeIdx]cluster.Probe{1: {Restores: 2}, 3: {Restores: 2}}
	var want []string
	for self := meta.NodeIdx(1); self <= 3; self++ {
		got := clusterRestores(context.Background(), nodes, probes, func(meta.NodeIdx) bool { return true }, self, byHost[self], func(_ context.Context, idx meta.NodeIdx) ([]rpc.RestoreJob, error) {
			return byHost[idx], nil
		})
		var ids []string
		for _, j := range got {
			ids = append(ids, j.ID)
		}
		if want == nil {
			want = ids
		} else if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Fatalf("node %d lists %v, node 1 %v", self, ids, want)
		}
	}
	if strings.Join(want, ",") != "1-1,3-2,3-1,1-2" {
		t.Fatalf("the order %v, want newest first", want)
	}
}

// A host runs one job at a time: a second job queues behind the first and runs once it
// is done.
//
// Invariant: I-366
func TestRestoreJob_OneJobPerHost_Queued(t *testing.T) {
	r := newJobRig(t)
	r.gate.hold(r.big)
	first := r.start(0, "/one")
	<-r.gate.blocked
	second := r.start(0, "/two")
	if st := r.job(second).State; st != rpc.RestoreQueued {
		t.Fatalf("the second job is %s while the first runs", st)
	}
	r.gate.open()
	a, b := r.await(first, rpc.RestoreDone), r.await(second, rpc.RestoreDone)
	if b.Ended < a.Ended {
		t.Fatal("the second job ended before the first")
	}
	r.matches("/one")
	r.matches("/two")
}

// A job writes as its starter and checks them again at every start: a user without a role
// where the copy lands fails at once, and a job whose starter is disabled while it runs
// fails when the host resumes it and is never tried again.
//
// Invariant: I-367
func TestRestoreJob_RunsAsStarter_RecheckedOnResume(t *testing.T) {
	r := newJobRig(t)
	kim, lee := r.f.Users()
	if err := r.fs.As(kim).MkdirAll("/w", 0o755); err != nil {
		t.Fatal(err)
	}
	r.f.Grant(kim, "/", lee, meta.RoleRead) // the snapshot's files are lee's to read (I-348)
	r.f.Grant(kim, "/w", lee, meta.RoleAdmin)
	denied := r.start(lee, "/elsewhere")
	if j := r.await(denied, rpc.RestoreFailed); j.Error == "" {
		t.Fatal("a failed job says nothing")
	}
	if _, err := r.f.FSM.Resolve([]string{"elsewhere"}); err == nil {
		t.Fatal("a refused job made its folder")
	}
	r.gate.hold(r.big)
	id := r.start(lee, "/w/back")
	<-r.gate.blocked
	now := r.f.Clk.Now().UnixNano()
	r.f.MustApply(meta.Command{Op: meta.OpUser, Actor: kim, UserOp: &meta.UserOp{Action: meta.UserDisable, Target: lee, Now: now}})
	r.restart()
	r.gate.open()
	j := r.await(id, rpc.RestoreFailed)
	if !strings.Contains(j.Error, "disabled") {
		t.Fatalf("the job failed for %q", j.Error)
	}
	r.f.Clk.Advance(restore.RetryMax + time.Minute)
	if got := r.job(id); got.State != rpc.RestoreFailed || got.Updated != j.Updated {
		t.Fatalf("a failed job was tried again: %+v", got)
	}
}

// Only a job's starter or an admin cancels it: another user is refused; the starter's
// cancel stops a running job and leaves no temporary file, the files it finished whole;
// an admin's cancels the next one; a finished job cannot be cancelled.
//
// Invariant: I-368
func TestRestoreJob_CancelByStarterOrAdmin(t *testing.T) {
	r := newJobRig(t)
	kim, lee := r.f.Users()
	if err := r.fs.As(kim).MkdirAll("/w", 0o755); err != nil {
		t.Fatal(err)
	}
	r.f.Grant(kim, "/", lee, meta.RoleRead) // the snapshot's files are lee's to read (I-348)
	r.f.Grant(kim, "/w", lee, meta.RoleAdmin)
	r.gate.hold(r.big)
	running := r.start(lee, "/w/back")
	<-r.gate.blocked
	queued := r.start(kim, "/later")
	ctx := context.Background()
	if err := r.jobs.Cancel(ctx, queued, lee); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("lee cancelled kim's job: %v", err)
	}
	if err := r.jobs.Cancel(ctx, running, lee); err != nil {
		t.Fatal(err)
	}
	r.await(running, rpc.RestoreCancelled)
	noTemp(t, r.fs.As(kim), "/w")
	if fis, err := r.fs.As(kim).ReadDir("/w/back"); err == nil {
		for _, fi := range fis {
			if !fi.IsDir() && fi.Name() == "a.bin" {
				t.Fatal("the file being written when cancelled has its name")
			}
		}
	}
	if err := r.jobs.Cancel(ctx, queued, kim); err != nil {
		t.Fatal(err)
	}
	r.await(queued, rpc.RestoreCancelled)
	if err := r.jobs.Cancel(ctx, queued, kim); !errors.Is(err, rpc.ErrNoRestore) {
		t.Fatalf("a second cancel: %v", err)
	}
	if _, err := os.Stat(path.Join(r.dir, restore.File)); err != nil {
		t.Fatalf("the jobs' file: %v", err)
	}
}

// runJobs is a host's restore jobs over the fabric, running until the test ends.
func runJobs(t *testing.T, f *testharness.Fabric, fs *vfs.FS, src *testharness.Mirrors) *restore.Jobs {
	t.Helper()
	jobs := restore.New(t.TempDir(), fs, f.FSM, func() (meta.NodeIdx, bool) { return f.SelfIdx, true }, src.Online, f.Clk, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { jobs.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return jobs
}

// awaitJob waits until the job a snapshot_restore started is in state.
func awaitJob(t *testing.T, jobs *restore.Jobs, reply any, state string) rpc.RestoreJob {
	t.Helper()
	started, ok := reply.(rpc.RestoreStarted)
	if !ok {
		t.Fatalf("snapshot_restore answered %T, want a job", reply)
	}
	var got rpc.RestoreJob
	testharness.Eventually(t, func() bool {
		list := jobs.List()
		for i := range list {
			if list[i].ID == started.Job {
				got = list[i]
			}
		}
		return got.State == state
	}, "job "+started.Job+" never reached "+state)
	return got
}
