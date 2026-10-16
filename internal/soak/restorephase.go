package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/users"
)

// --- 32 restore jobs -----------------------------------------------------------------------
//
// A tree restore is a job on the mirror's host (docs/16 §6). The phase plugs the
// environment's scratch mirror back in, writes a corpus of a few hundred files and takes
// a snapshot of it, then restores it through a gateway that is not the mirror's host: the
// start answers at once, every node's status lists the job with its totals before its
// first chunk and done counts that never fall, and every restored file is the
// snapshot's. With the mirror unplugged the next restores are certainly unfinished
// whatever the environment's speed: one waits, survives its host's restart and, once the
// mirror is back, completes; two more queue one behind the other; one is cancelled and
// leaves no temporary file; and, once the cluster has users, a user who did not start a
// job may not cancel it. A live cluster's restores are only read: every node lists the
// same jobs.

// The phase's report records (report_restores.go draws them): a restoreSample per look at
// a job, a restoreMark for what was done to the cluster meanwhile, a restoreRow per job.
const (
	restoreSample = "restore sample"
	restoreMark   = "restore mark"
	restoreRow    = "restore job"
)

// restoreFolders × restoreFilesEach small files and two multi-chunk ones: a few hundred.
const (
	restoreFolders   = 8
	restoreFilesEach = 30
)

func phaseRestoreJobs(ctx context.Context, r *Run) error {
	gws := r.gateways()
	if len(gws) < 2 {
		r.skip("fewer than two gateways: a restore through a node that is not the mirror's host needs two")
		return nil
	}
	if !r.Env.Caps().Mirror {
		return r.restoresReadOnly(ctx, gws)
	}
	if !r.Env.Caps().Mutate {
		r.skip("restore jobs write the volume and restart the mirror's host: only with Caps.Mutate")
		return nil
	}
	j := &jobRun{r: r, seen: map[string]rpc.RestoreJob{}, labels: map[string]string{}}
	return j.run(ctx, gws)
}

// restoresReadOnly is the phase on a live cluster: every node lists the same jobs.
func (r *Run) restoresReadOnly(ctx context.Context, gws []NodeRef) error {
	var first []string
	for i, g := range gws {
		rep, err := r.Env.Status(ctx, g)
		if err != nil {
			r.checkAt("I-365", "status.restores on "+g.Name, false, err.Error(), "", "", g.Name)
			continue
		}
		ids := make([]string, 0, len(rep.Restores))
		for k := range rep.Restores {
			ids = append(ids, rep.Restores[k].ID)
		}
		if i == 0 {
			first = ids
			continue
		}
		r.check("I-365", "every node lists the same restore jobs: "+g.Name, slices.Equal(ids, first), fmt.Sprintf("%v against %v", ids, first))
	}
	return nil
}

// jobRun is phase 32 on the environment's scratch mirror.
type jobRun struct {
	r      *Run
	m      *mirrorRun
	via    NodeRef // the gateway asked: not the mirror's host
	snap   string
	dir    string
	corpus model
	seen   map[string]rpc.RestoreJob // the last look at every job, by ID
	labels map[string]string         // job ID → its role in the phase
}

func (j *jobRun) run(ctx context.Context, gws []NodeRef) error {
	r := j.r
	name, host, err := r.Env.AddMirror(ctx)
	if err != nil || name == "" {
		r.skip(fmt.Sprintf("the environment has no scratch mirror (%v)", err))
		return nil
	}
	j.m = &mirrorRun{r: r, name: name, host: host, writer: gws[0], labels: map[string]string{}, reads: map[string]string{}}
	for _, g := range gws {
		if g.Idx != host.Idx {
			j.via = g
		}
	}
	if j.via.Idx == host.Idx || j.via.Name == "" {
		r.skip("every gateway is the mirror's host")
		return nil
	}
	if j.m.writer.Idx == host.Idx {
		j.m.writer = j.via
	}
	u := r.undoer(ctx)
	defer u.run()
	if err := r.Env.SetMirrorOnline(ctx, name, true); err != nil {
		r.check("I-188", "the scratch mirror plugged back in", false, err.Error())
		return nil
	}
	u.add("unplug the mirror for the later phases", func(c context.Context) error { return r.Env.SetMirrorOnline(c, name, false) })
	if !r.poll(ctx, 3*time.Minute, 2*time.Second, func() bool { return j.m.online() == 1 }) {
		r.check("I-188", "the mirror is online on its host", false, j.m.mirrorReason())
		return nil
	}

	// the corpus, then its snapshot
	j.dir = join(r.Root, "restore-jobs")
	j.corpus = model{}
	for f := range restoreFolders {
		for k := range restoreFilesEach {
			j.corpus[join(j.dir, fmt.Sprintf("f%d", f), fmt.Sprintf("file%02d.dat", k))] = r.randomBytes(500 + r.Rng.Intn(4000))
		}
	}
	cs := r.Profile.ChunkSize
	j.corpus[join(j.dir, "big1.bin")] = r.randomBytes(2*cs + 77)
	j.corpus[join(j.dir, "big2.bin")] = r.randomBytes(cs + 5)
	if err := r.writeModel(r.volume(j.m.writer), j.corpus); err != nil {
		r.checkAt("I-7", "restore corpus written", false, err.Error(), j.dir, "", j.m.writer.Name)
		return nil
	}
	if !j.m.drained(ctx) {
		return nil
	}
	j.snap = fmt.Sprintf("restore-%d", time.Now().UnixNano())
	if _, ok := j.m.take(ctx, j.snap); !ok {
		return nil
	}

	j.first(ctx, host)
	j.unplugged(ctx, name, host)
	j.recordRows()
	return nil
}

// start asks j.via for a restore of src under to and times the answer.
func (j *jobRun) start(ctx context.Context, src, to, label string) (rpc.RestoreStarted, time.Duration, error) {
	t0 := time.Now()
	raw, err := j.r.Env.SnapshotOp(ctx, j.via, rpc.AdminOp{Op: "snapshot_restore", Mirror: j.m.name, Name: j.snap, Path: src, To: to})
	took := time.Since(t0)
	var st rpc.RestoreStarted
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err == nil {
		j.labels[st.Job] = label
	}
	return st, took, err
}

// look is job id as node n's status lists it.
func (j *jobRun) look(ctx context.Context, n NodeRef, id string) (rpc.RestoreJob, bool) {
	rep, err := j.r.Env.Status(ctx, n)
	if err != nil {
		return rpc.RestoreJob{}, false
	}
	for k := range rep.Restores {
		if rep.Restores[k].ID == id {
			return rep.Restores[k], true
		}
	}
	return rpc.RestoreJob{}, false
}

// follow looks at job id through j.via until done holds or timeout: every look is a
// report sample, and a done count below an earlier one is a fall (I-365).
func (j *jobRun) follow(ctx context.Context, id string, timeout time.Duration, done func(rpc.RestoreJob) bool) (last rpc.RestoreJob, ok, fell bool) {
	var prev rpc.RestoreJob
	ok = j.r.poll(ctx, timeout, time.Second, func() bool {
		job, found := j.look(ctx, j.via, id)
		if !found {
			return false
		}
		if job.BytesDone < prev.BytesDone || job.FilesDone < prev.FilesDone {
			fell = true
		}
		prev, last = job, job
		j.seen[id] = job
		j.sample(job)
		return done(job)
	})
	return last, ok, fell
}

func (j *jobRun) sample(job rpc.RestoreJob) {
	j.r.emit(Record{Kind: "event", Name: restoreSample, Text: job.ID, Attrs: map[string]string{
		"label": j.labels[job.ID], "state": job.State, "bytes": strconv.FormatInt(job.BytesDone, 10), "total": strconv.FormatInt(job.BytesTotal, 10),
	}})
}

func (j *jobRun) mark(label string) {
	j.r.emit(Record{Kind: "event", Name: restoreMark, Text: label})
}

func finished(job rpc.RestoreJob) bool {
	switch job.State {
	case rpc.RestoreDone, rpc.RestoreFailed, rpc.RestoreCancelled:
		return true
	}
	return false
}

func running(job rpc.RestoreJob) bool {
	return job.State == rpc.RestoreRunning || job.State == rpc.RestoreCounting
}

// restoredModel is the corpus as a restore under to lays it out.
func (j *jobRun) restoredModel(to string) model {
	out := model{}
	base := j.dir[strings.LastIndex(j.dir, "/")+1:]
	for p, b := range j.corpus {
		out[join(to, base)+strings.TrimPrefix(p, j.dir)] = b
	}
	return out
}

// verify checks every file restored under to against the snapshot's bytes.
func (j *jobRun) verify(to, label string) {
	want := j.restoredModel(to)
	bad, first := j.r.verifyModel(j.r.volume(j.via), want)
	j.r.checkAt("I-363 I-364", "every file of the "+label+" restore is the snapshot's", bad == 0,
		fmt.Sprintf("%d of %d files differ", bad, len(want)), first, "", j.via.Name)
}

// first: the start answers at once, every node lists the job with totals before its
// first chunk, its done counts never fall, and every restored file is the snapshot's.
func (j *jobRun) first(ctx context.Context, host NodeRef) {
	r := j.r
	to := join(r.Root, "restored-a")
	st, took, err := j.start(ctx, j.dir, to, "first")
	r.check("I-363", "a restore through "+j.via.Name+" answers once its job is recorded on the mirror's host", err == nil && st.Job != "" && st.Host == host.Name && took < 30*time.Second,
		fmt.Sprintf("job %s on %s in %s (%v)", st.Job, st.Host, took.Round(time.Millisecond), err))
	if err != nil {
		return
	}
	all := r.poll(ctx, time.Minute, time.Second, func() bool {
		for _, n := range r.gateways() {
			if _, ok := j.look(ctx, n, st.Job); !ok {
				return false
			}
		}
		return true
	})
	r.check("I-365", "every node's status lists the job", all, st.Job)
	counted, _, _ := j.follow(ctx, st.Job, 2*time.Minute, func(job rpc.RestoreJob) bool { return job.Counted || finished(job) })
	var wantBytes int64
	for _, b := range j.corpus {
		wantBytes += int64(len(b))
	}
	r.check("I-365", "the job's totals are the snapshot's files and bytes", counted.FilesTotal == int64(len(j.corpus)) && counted.BytesTotal == wantBytes,
		fmt.Sprintf("%d files, %s; the corpus %d files, %s", counted.FilesTotal, humanBytes(float64(counted.BytesTotal)), len(j.corpus), humanBytes(float64(wantBytes))))
	last, ok, fell := j.follow(ctx, st.Job, 10*time.Minute, finished)
	r.check("I-365", "the job's done counts never fall and end at its totals", !fell && last.FilesDone == last.FilesTotal && last.BytesDone == last.BytesTotal,
		fmt.Sprintf("%d of %d files, %s of %s", last.FilesDone, last.FilesTotal, humanBytes(float64(last.BytesDone)), humanBytes(float64(last.BytesTotal))))
	r.check("I-363", "the job completes", ok && last.State == rpc.RestoreDone, fmt.Sprintf("%s %s", last.State, last.Error))
	j.verify(to, "first")
}

// unplugged: with the mirror away a restore waits, survives its host's restart and
// completes once the mirror is back; two queue one behind the other; one is cancelled;
// another user's cancel is refused (I-364, I-366, I-368).
func (j *jobRun) unplugged(ctx context.Context, mirrorName string, host NodeRef) {
	r := j.r
	j.mark("mirror unplugged")
	if err := r.Env.SetMirrorOnline(ctx, mirrorName, false); err != nil {
		r.check("I-364", "the mirror unplugged", false, err.Error())
		return
	}
	plugged := false
	plug := func() {
		if !plugged {
			plugged = true
			j.mark("mirror back")
			if err := r.Env.SetMirrorOnline(ctx, mirrorName, true); err != nil {
				r.check("I-364", "the mirror plugged back in", false, err.Error())
			}
		}
	}
	defer plug()
	toB := join(r.Root, "restored-b")
	b, _, err := j.start(ctx, j.dir, toB, "waited and resumed")
	if err != nil {
		r.check("I-363", "a restore starts while its mirror is unplugged", false, err.Error())
		return
	}
	waited, _, _ := j.follow(ctx, b.Job, 3*time.Minute, func(job rpc.RestoreJob) bool { return job.State == rpc.RestoreWaiting || finished(job) })
	r.check("I-364", "a job waits while its mirror is offline", waited.State == rpc.RestoreWaiting, fmt.Sprintf("%s %s", waited.State, waited.Error))
	j.mark("host restarted")
	if err := r.Env.Restart(ctx, []NodeRef{host}); err != nil {
		r.check("I-364", "the mirror's host restarts", false, err.Error())
		return
	}
	after, _, _ := j.follow(ctx, b.Job, 3*time.Minute, func(job rpc.RestoreJob) bool { return job.State != "" })
	r.check("I-364", "the recorded job survives its host's restart", after.ID == b.Job && !finished(after), fmt.Sprintf("%s after the restart", after.State))

	// two more, one behind the other, and one cancelled: all before the mirror is back
	src := join(j.dir, "f0")
	c, _, err1 := j.start(ctx, src, join(r.Root, "restored-c"), "first of two")
	d, _, err2 := j.start(ctx, src, join(r.Root, "restored-d"), "queued")
	toE := join(r.Root, "restored-e")
	e, _, err3 := j.start(ctx, j.dir, toE, "cancelled")
	if err := errors.Join(err1, err2, err3); err != nil {
		r.check("I-366", "three more restores start", false, err.Error())
		return
	}
	j.mark("cancel")
	_, cerr := r.Env.SnapshotOp(ctx, j.via, rpc.AdminOp{Op: "restore_cancel", Job: e.Job})
	ce, _, _ := j.follow(ctx, e.Job, time.Minute, finished)
	r.check("I-368", "the starter's cancel stops the job", cerr == nil && ce.State == rpc.RestoreCancelled, fmt.Sprintf("%s (%v)", ce.State, cerr))
	j.otherUserRefused(ctx)

	plug()
	last, ok, _ := j.follow(ctx, b.Job, 10*time.Minute, finished)
	r.check("I-364", "the job goes on once the mirror is back, after its host's restart, and completes", ok && last.State == rpc.RestoreDone && last.FilesDone == last.FilesTotal,
		fmt.Sprintf("%s %s, %d of %d files", last.State, last.Error, last.FilesDone, last.FilesTotal))
	j.verify(toB, "restarted")
	j.queued(ctx, c.Job, d.Job)
	left := j.tempsUnder(toE)
	r.check("I-368", "a cancelled job leaves no temporary file", len(left) == 0, fmt.Sprintf("%v", left))
}

// queued: one host runs one job at a time, the second after the first (I-366).
func (j *jobRun) queued(ctx context.Context, c, d string) {
	r := j.r
	overlap, sawQueued := false, false
	var lc, ld rpc.RestoreJob
	ok := r.poll(ctx, 10*time.Minute, time.Second, func() bool {
		rep, err := r.Env.Status(ctx, j.via) // both from one report: two looks would straddle a job's end
		if err != nil {
			return false
		}
		var f1, f2 bool
		for k := range rep.Restores {
			switch rep.Restores[k].ID {
			case c:
				lc, f1 = rep.Restores[k], true
			case d:
				ld, f2 = rep.Restores[k], true
			}
		}
		if !f1 || !f2 {
			return false
		}
		j.seen[c], j.seen[d] = lc, ld
		j.sample(lc)
		j.sample(ld)
		if running(lc) && running(ld) {
			overlap = true
		}
		if !finished(lc) && !running(ld) && !finished(ld) {
			sawQueued = true
		}
		return finished(lc) && finished(ld)
	})
	r.check("I-366", "a second job queues behind the first and runs after it", ok && !overlap && lc.State == rpc.RestoreDone && ld.State == rpc.RestoreDone && lc.Ended <= ld.Ended,
		fmt.Sprintf("both running at once: %v, the second seen waiting its turn: %v, %s then %s", overlap, sawQueued, lc.State, ld.State))
}

// tempsUnder lists every .tailraid-restore- name under dir (absent dirs are empty).
func (j *jobRun) tempsUnder(dir string) []string {
	v := j.r.volume(j.via)
	var out []string
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		names, err := v.List(d)
		if err != nil || depth > 3 {
			return
		}
		for _, n := range names {
			if strings.HasPrefix(n, ".tailraid-restore-") {
				out = append(out, join(d, n))
			}
			if !strings.Contains(n, ".") {
				walk(join(d, n), depth+1)
			}
		}
	}
	walk(dir, 0)
	return out
}

// otherUserRefused: once the cluster has users, a user who did not start a job may not
// cancel it, and the job goes on (I-368).
func (j *jobRun) otherUserRefused(ctx context.Context) {
	r := j.r
	su := r.soakUsers()
	if su == nil || su.kim == nil {
		r.checkSkipped("I-368", "another user's cancel is refused", "the cluster has no users yet in this run (users first runs it; TestRestoreJob_CancelByStarterOrAdmin covers the rule)")
		return
	}
	n := su.nodes[0]
	who := fmt.Sprintf("rae%04d", r.Rng.Intn(10000))
	code, err := su.invite(ctx, n, su.kim, who)
	var rae *users.Identity
	if err == nil {
		rae, err = su.create(ctx, r, n, who, code)
	}
	if err != nil {
		r.check("I-368", "a second user for the cancel check", false, err.Error())
		return
	}
	defer func() { _ = su.op(ctx, n, su.kim, rpc.AdminOp{Op: "user_rm", User: who}, nil) }()
	st, _, err := j.start(ctx, join(j.dir, "f1"), join(r.Root, "restored-f"), "another user's cancel")
	if err != nil {
		r.check("I-368", "a restore for the cancel check starts", false, err.Error())
		return
	}
	cerr := su.op(ctx, j.via, rae, rpc.AdminOp{Op: "restore_cancel", Job: st.Job}, nil)
	still, _ := j.look(ctx, j.via, st.Job)
	r.check("I-368", "a user who did not start a job may not cancel it", refused(cerr) && !finished(still), fmt.Sprintf("%s's cancel of %s: %v; the job %s", who, st.Job, cerr, still.State))
}

// recordRows emits one row per job for the report's table.
func (j *jobRun) recordRows() {
	ids := make([]string, 0, len(j.seen))
	for id := range j.seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		job := j.seen[id]
		took := ""
		if job.Ended > 0 && job.Started > 0 {
			took = time.Duration(job.Ended - job.Started).Round(time.Millisecond).String()
		}
		j.r.emit(Record{Kind: "event", Name: restoreRow, Text: id, Attrs: map[string]string{
			"label": j.labels[id], "dst": job.Dst, "state": job.State, "files": fmt.Sprintf("%d of %d", job.FilesDone, job.FilesTotal),
			"bytes": humanBytes(float64(job.BytesDone)), "existing": strconv.FormatInt(job.Existing, 10), "resumed": strconv.Itoa(job.Resumed), "took": took,
		}})
	}
}
