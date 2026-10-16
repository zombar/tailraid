package node

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/vfs"
)

// The restore jobs' bounds (docs/16 §6, §8).
const (
	restoreFile      = "restores.json"
	restoreSaveEvery = 30 * time.Second   // the counters on disk, besides every change of state
	restoreRetryMin  = time.Minute        // a transient failure's first wait, doubling
	restoreRetryMax  = 10 * time.Minute   // and its longest
	restoreWaitCheck = time.Minute        // how often a waiting job looks for its mirror
	restoreKeep      = 7 * 24 * time.Hour // a finished job stays listed this long
	restoreKeepMax   = 20                 // and at most this many finished jobs
	restoreRateOver  = time.Minute        // the rate's window
)

// restoreJobs is this host's restore jobs (docs/16 §6): recorded in <data_dir>/restores.json,
// run one at a time in the order they came, each as the user who started it, resumed at
// start. A job reads its mirror from this host's own disk.
type restoreJobs struct {
	file   string
	fs     *vfs.FS // the node's own view; a job runs on its starter's (fs.As)
	st     *meta.Store
	self   func() (meta.NodeIdx, bool)
	online func(meta.NodeIdx) bool // a mirror open on this host
	clk    clock.Clock
	log    *slog.Logger

	mu      sync.Mutex
	jobs    []*restoreJob // in the order they came
	next    int           // the next job number on this host
	loaded  bool
	kick    chan struct{}
	saved   time.Time
	cancels map[string]context.CancelFunc // the running job's, by ID
}

// restoreJob is a job's record and what only the running process knows of it.
type restoreJob struct {
	rpc.RestoreJob
	cancelled bool         // cancel was asked while it ran
	runFiles  int64        // this run's done counts: a resume starts again from what exists
	runBytes  int64        //
	runExist  int64        //
	samples   []rateSample // bytes done over the last minute
}

type rateSample struct {
	at    time.Time
	bytes int64
}

// restoreState is the file: the jobs and the next job number.
type restoreState struct {
	Next int              `json:"next"`
	Jobs []rpc.RestoreJob `json:"jobs"`
}

func newRestoreJobs(dataDir string, fs *vfs.FS, st *meta.Store, self func() (meta.NodeIdx, bool), online func(meta.NodeIdx) bool, clk clock.Clock, log *slog.Logger) *restoreJobs {
	return &restoreJobs{
		file: path.Join(dataDir, restoreFile), fs: fs, st: st, self: self, online: online, clk: clk, log: log,
		kick: make(chan struct{}, 1), cancels: map[string]context.CancelFunc{},
	}
}

// load reads the file once; a job the last run left running or counting is resumed, the
// others keep their state (I-364).
func (j *restoreJobs) load() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.loaded {
		return
	}
	j.loaded = true
	b, err := os.ReadFile(j.file)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			j.log.Warn("restore jobs not read", "file", j.file, "err", err)
		}
		return
	}
	var s restoreState
	if err := json.Unmarshal(b, &s); err != nil {
		j.log.Warn("restore jobs not read", "file", j.file, "err", err)
		return
	}
	j.next = s.Next
	for i := range s.Jobs {
		job := &restoreJob{RestoreJob: s.Jobs[i]}
		if job.State == rpc.RestoreRunning || job.State == rpc.RestoreCounting {
			job.State = rpc.RestoreQueued
			job.Resumed++
		}
		j.jobs = append(j.jobs, job)
	}
}

// saveLocked writes the file (temp, fsync, rename, directory fsync).
func (j *restoreJobs) saveLocked() {
	s := restoreState{Next: j.next, Jobs: make([]rpc.RestoreJob, 0, len(j.jobs))}
	for _, job := range j.jobs {
		s.Jobs = append(s.Jobs, job.RestoreJob)
	}
	b, err := json.Marshal(s)
	if err == nil {
		err = sysfs.WriteFileSync(j.file, b)
	}
	if err != nil {
		j.log.Warn("restore jobs not saved", "file", j.file, "err", err)
	}
	j.saved = j.clk.Now()
}

// setLocked moves job to state and records it at once.
func (j *restoreJobs) setLocked(job *restoreJob, state, why string) {
	now := j.clk.Now()
	job.State, job.Error, job.Updated = state, why, now.UnixNano()
	if job.Finished() {
		job.Ended, job.NextTry = now.UnixNano(), 0
	}
	j.pruneLocked(now)
	j.saveLocked()
}

// pruneLocked drops the finished jobs older than restoreKeep, and the oldest finished
// beyond restoreKeepMax.
func (j *restoreJobs) pruneLocked(now time.Time) {
	finished := 0
	for _, job := range j.jobs {
		if job.Finished() {
			finished++
		}
	}
	out := j.jobs[:0]
	for _, job := range j.jobs {
		if job.Finished() && (now.Sub(time.Unix(0, job.Ended)) > restoreKeep || finished > restoreKeepMax) {
			finished--
			continue
		}
		out = append(out, job)
	}
	j.jobs = out
}

// List is the jobs, oldest first, with each one's rate and its user's name.
func (j *restoreJobs) List() []rpc.RestoreJob {
	j.load()
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]rpc.RestoreJob, 0, len(j.jobs))
	now := j.clk.Now()
	for _, job := range j.jobs {
		r := job.RestoreJob
		if job.Actor != 0 {
			if u, ok := j.st.User(job.Actor); ok {
				r.User = u.Name
			}
		}
		if r.State == rpc.RestoreRunning {
			r.Rate = job.rate(now)
		}
		out = append(out, r)
	}
	return out
}

// held is the snapshots an unfinished job reads: thinning and the reserve leave them (I-364).
func (j *restoreJobs) held() map[meta.SnapID]bool {
	j.load()
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[meta.SnapID]bool{}
	for _, job := range j.jobs {
		if !job.Finished() {
			out[job.SnapID] = true
		}
	}
	return out
}

// Start records a job on this host, the mirror's, and returns: nothing about the request
// that asked touches the job from here on (I-363). The node the user asked checked their
// role where the copy lands; the job checks it again when it runs (I-367).
func (j *restoreJobs) Start(_ context.Context, req rpc.RestoreStart) (rpc.RestoreStarted, error) {
	j.load()
	self, ok := j.self()
	row, found := j.st.Node(req.Mirror)
	if !ok || !found || row.Kind != meta.KindMirror || row.Host != self {
		return rpc.RestoreStarted{}, rpc.ErrNotHost
	}
	sn, ok := snapshotOf(j.st, req.Mirror, req.Snapshot)
	if !ok {
		return rpc.RestoreStarted{}, fmt.Errorf("snapshot %d of %s: %w", req.Snapshot, row.Name, meta.ErrNotExist)
	}
	host, _ := j.st.Node(self)
	now := j.clk.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.next++
	job := &restoreJob{
		ID: fmt.Sprintf("%d-%d", self, j.next), Host: host.Name, Mirror: row.Name, MirrorIdx: row.Idx, Snapshot: sn.Name, SnapID: sn.ID,
		Src: path.Join("/", req.Path), Dst: path.Join("/", req.Dst), Actor: req.Actor, State: rpc.RestoreQueued,
		Started: now.UnixNano(), Updated: now.UnixNano(),
	}
	j.jobs = append(j.jobs, job)
	j.saveLocked()
	select {
	case j.kick <- struct{}{}:
	default:
	}
	return rpc.RestoreStarted{Job: job.ID, Host: host.Name, Path: job.Dst}, nil
}

// Cancel stops job id for actor, its starter or an admin (before the first user, anyone):
// a queued, waiting or retrying job ends at once, a running one before its next chunk,
// removing the file it was writing (I-368).
func (j *restoreJobs) Cancel(_ context.Context, id string, actor meta.UserIdx) error {
	j.load()
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.jobs {
		if job.ID != id || job.Finished() {
			continue
		}
		if j.st.Enforcing() && actor != job.Actor && !j.st.IsAdmin(actor) {
			return fmt.Errorf("cancel restore %s: only who started it or an admin: %w", id, meta.ErrAccess)
		}
		if cancel, running := j.cancels[id]; running {
			job.cancelled = true
			cancel()
			return nil
		}
		j.setLocked(job, rpc.RestoreCancelled, "")
		return nil
	}
	return fmt.Errorf("%w: %s", rpc.ErrNoRestore, id)
}

// Run resumes the recorded jobs and runs each in turn until ctx ends: the oldest that can
// run (a waiting one once its mirror is back, a retrying one once its time came), one at
// a time (I-366).
func (j *restoreJobs) Run(ctx context.Context) {
	j.load()
	for {
		job, wait := j.pick()
		if job != nil {
			j.run(ctx, job)
			continue
		}
		t := j.clk.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-j.kick:
		case <-t.C():
		}
		t.Stop()
	}
}

// pick is the job to run now, or how long until one may run.
func (j *restoreJobs) pick() (*restoreJob, time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.clk.Now()
	wait := restoreWaitCheck
	for _, job := range j.jobs {
		switch job.State {
		case rpc.RestoreQueued:
			return job, 0
		case rpc.RestoreWaiting:
			if j.online(job.MirrorIdx) {
				return job, 0
			}
		case rpc.RestoreRetrying:
			at := time.Unix(0, job.NextTry)
			if !now.Before(at) {
				return job, 0
			}
			wait = min(wait, at.Sub(now))
		}
	}
	return nil, wait
}

// run is one attempt at job: the checks, the count, the copy, and the state it ends in.
func (j *restoreJobs) run(ctx context.Context, job *restoreJob) {
	rctx, cancel := context.WithCancel(chunk.Background(ctx)) // at background priority: client traffic keeps its share (I-175)
	defer cancel()
	j.mu.Lock()
	j.cancels[job.ID] = cancel
	job.runFiles, job.runBytes, job.runExist, job.samples = 0, 0, 0, nil
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		delete(j.cancels, job.ID)
		j.mu.Unlock()
	}()
	err := j.attempt(rctx, job)
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.clk.Now()
	switch {
	case job.cancelled:
		j.setLocked(job, rpc.RestoreCancelled, "")
	case err == nil:
		job.FilesDone, job.BytesDone = max(job.FilesDone, job.FilesTotal), max(job.BytesDone, job.BytesTotal)
		j.setLocked(job, rpc.RestoreDone, "")
	case ctx.Err() != nil:
		job.State = rpc.RestoreRunning // the node is stopping: resumed at the next start (I-364)
		j.saveLocked()
	case !j.online(job.MirrorIdx):
		j.setLocked(job, rpc.RestoreWaiting, "the mirror "+job.Mirror+" is offline")
	case permanent(err):
		j.log.Warn("restore failed", "job", job.ID, "err", err)
		j.setLocked(job, rpc.RestoreFailed, err.Error())
	default:
		delay := min(restoreRetryMax, restoreRetryMin<<min(job.Tries, 4))
		job.Tries++
		job.NextTry = now.Add(delay).UnixNano()
		j.log.Info("restore retrying", "job", job.ID, "in", delay, "err", err)
		j.setLocked(job, rpc.RestoreRetrying, err.Error())
	}
}

// attempt runs the job once as its starter: the user still there and enabled, the mirror
// open, the snapshot in the catalogue, the totals counted once, then the tree restored at
// RestoreFiles files and RestoreInFlight chunks, its progress kept so the done counts
// never fall across attempts (I-365, I-367). The restore checks Admin where the copy lands.
func (j *restoreJobs) attempt(ctx context.Context, job *restoreJob) error {
	fs := j.fs
	if job.Actor != 0 {
		u, ok := j.st.User(job.Actor)
		switch {
		case !ok || u.State == meta.UserRemoved:
			return permanentError{errors.New("the user who started it was removed")}
		case u.State == meta.UserDisabled:
			return permanentError{fmt.Errorf("%s is disabled", u.Name)}
		}
		fs = j.fs.As(job.Actor)
	}
	if !j.online(job.MirrorIdx) {
		return vfs.ErrMirrorOffline
	}
	if _, ok := snapshotOf(j.st, job.MirrorIdx, job.SnapID); !ok {
		return permanentError{fmt.Errorf("the snapshot %s is gone", job.Snapshot)}
	}
	src := snapshotPath(job.Mirror, job.Snapshot, job.Src)
	if !job.Counted {
		j.mu.Lock()
		j.setLocked(job, rpc.RestoreCounting, "")
		j.mu.Unlock()
		files, bytes, err := fs.RestoreTotals(ctx, src, job.Dst)
		if errors.Is(err, os.ErrNotExist) {
			return permanentError{fmt.Errorf("the snapshot %s holds nothing at %s", job.Snapshot, job.Src)}
		}
		if err != nil {
			return err
		}
		j.mu.Lock()
		job.FilesTotal, job.BytesTotal, job.Counted = files, bytes, true
		j.mu.Unlock()
	}
	j.mu.Lock()
	j.setLocked(job, rpc.RestoreRunning, "")
	j.mu.Unlock()
	_, err := fs.RestoreSnapshotWith(ctx, src, job.Dst, vfs.RestoreOptions{
		Files: vfs.RestoreFiles, InFlight: vfs.RestoreInFlight,
		Progress: func(p vfs.RestoreProgress) { j.progress(job, p) },
	})
	if errors.Is(err, os.ErrExist) && job.FilesTotal <= 1 {
		return nil // a single file the last attempt finished before its state was saved
	}
	return err
}

// progress adds one step to this attempt's counts; the job shows the larger of what it
// had and what this attempt reached, since a resumed attempt counts what exists again.
func (j *restoreJobs) progress(job *restoreJob, p vfs.RestoreProgress) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job.runFiles += p.Files
	job.runBytes += p.Bytes
	if p.Existing {
		job.runExist += p.Files
	}
	job.FilesDone = min(max(job.FilesDone, job.runFiles), job.FilesTotal)
	job.BytesDone = min(max(job.BytesDone, job.runBytes), job.BytesTotal)
	job.Existing = max(job.Existing, job.runExist)
	if p.Bytes > 0 || p.Files > 0 {
		job.Tries = 0
	}
	now := j.clk.Now()
	job.Updated = now.UnixNano()
	job.samples = append(job.samples, rateSample{now, job.runBytes})
	for len(job.samples) > 2 && now.Sub(job.samples[0].at) > restoreRateOver {
		job.samples = job.samples[1:]
	}
	if now.Sub(j.saved) >= restoreSaveEvery {
		j.saveLocked()
	}
}

// rate is the bytes a second over the samples of the last minute.
func (job *restoreJob) rate(now time.Time) int64 {
	if len(job.samples) < 2 {
		return 0
	}
	first, last := job.samples[0], job.samples[len(job.samples)-1]
	if now.Sub(last.at) > restoreRateOver {
		return 0
	}
	dt := last.at.Sub(first.at).Seconds()
	if dt <= 0 {
		return 0
	}
	return int64(float64(last.bytes-first.bytes) / dt)
}

// permanentError is a failure waiting does not mend: the job fails and is not retried.
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// permanent reports a failure a later attempt would meet again: the starter gone or
// disabled, no role where the copy lands, a protected folder, a file where a folder goes,
// the snapshot gone (docs/16 §6).
func permanent(err error) bool {
	var p permanentError
	return errors.As(err, &p) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, meta.ErrAccess) || errors.Is(err, meta.ErrNotDir)
}

// snapshotOf is the catalogue's readable snapshot id of mirror.
func snapshotOf(st *meta.Store, mirror meta.NodeIdx, id meta.SnapID) (meta.Snapshot, bool) {
	for _, sn := range st.Snapshots() {
		if sn.ID == id && sn.Mirror == mirror && (sn.State == meta.SnapComplete || sn.State == meta.SnapIncomplete) {
			return sn, true
		}
	}
	return meta.Snapshot{}, false
}

// restoreHost is the host of job id, from its "<host idx>-<n>".
func restoreHost(id string) (meta.NodeIdx, bool) {
	h, _, ok := strings.Cut(id, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(h, 10, 16)
	return meta.NodeIdx(n), err == nil && n > 0
}

// sortRestores orders jobs of several hosts the same on every node: newest first, then
// by ID.
func sortRestores(jobs []rpc.RestoreJob) {
	slices.SortFunc(jobs, func(a, b rpc.RestoreJob) int {
		return cmp.Or(cmp.Compare(b.Started, a.Started), strings.Compare(a.ID, b.ID))
	})
}

// clusterRestores is every restore job of every live host, the same list on every node
// (I-365): this host's from its own list, another's from its GET /v1/restores while its
// last /health counted any. A host that does not answer in time is left out until the next
// build.
func clusterRestores(ctx context.Context, nodes []meta.Node, probes map[meta.NodeIdx]cluster.Probe, live func(meta.NodeIdx) bool, self meta.NodeIdx, local []rpc.RestoreJob, fetch func(ctx context.Context, idx meta.NodeIdx) ([]rpc.RestoreJob, error)) []rpc.RestoreJob {
	out := slices.Clone(local)
	for i := range nodes {
		n := &nodes[i]
		if n.Kind != meta.KindDevice || n.Idx == self || probes[n.Idx].Restores == 0 || !live(n.Idx) {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, restoreFetchTimeout)
		jobs, err := fetch(fctx, n.Idx)
		cancel()
		if err == nil {
			out = append(out, jobs...)
		}
	}
	sortRestores(out)
	return out
}

// restoreFetchTimeout bounds one host's GET /v1/restores in a status build.
const restoreFetchTimeout = 2 * time.Second
