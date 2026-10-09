package soak

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
)

// fakeEnv is the smallest Env the driver's own rules can be tested against: one volume in
// memory, a status report with a policy and a chunk size, every fault and admin call
// recorded in order and answered with nil. statusErr makes status fail (an abort), dump
// is what every node's goroutine dump returns.
type fakeEnv struct {
	mu        sync.Mutex
	calls     []string
	nodes     []NodeRef
	fs        Volume
	caps      Caps
	statusErr error
	dump      string
}

func newFakeEnv(n int) *fakeEnv {
	e := &fakeEnv{fs: BillyVolume{FS: memfs.New()}, dump: "goroutine 1 [running]:\nfake\n"}
	for i := 1; i <= n; i++ {
		e.nodes = append(e.nodes, NodeRef{Name: fmt.Sprintf("n%d", i), Idx: meta.NodeIdx(i), RPC: fmt.Sprintf("10.0.0.%d:1", i)})
	}
	return e
}

func (e *fakeEnv) record(ctx context.Context, what string) {
	live := "live"
	if ctx.Err() != nil {
		live = "dead"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, what+" ("+live+" ctx)")
}

func (e *fakeEnv) log() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *fakeEnv) Name() string                                    { return "fake" }
func (e *fakeEnv) Nodes(context.Context) ([]NodeRef, error)        { return e.nodes, nil }
func (e *fakeEnv) Volume(NodeRef) (Volume, bool)                   { return e.fs, true }
func (e *fakeEnv) Caps() Caps                                      { return e.caps }
func (e *fakeEnv) Wait(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func (e *fakeEnv) Status(_ context.Context, n NodeRef) (admin.Report, error) {
	if e.statusErr != nil {
		return admin.Report{}, e.statusErr
	}
	rep := admin.Report{Node: n.Name, Leader: "n1", Health: "healthy", Volume: admin.VolumeReport{K: 2, M: 1, ChunkSize: 4096}}
	for _, m := range e.nodes {
		rep.Nodes = append(rep.Nodes, admin.NodeReport{Idx: m.Idx, Name: m.Name, Kind: "device", State: "active", Voter: true, Leader: m.Idx == 1, Live: true})
	}
	return rep, nil
}

func (e *fakeEnv) RepairNow(context.Context) (repair.Stats, error) { return repair.Stats{}, nil }
func (e *fakeEnv) Scrub(context.Context) (repair.Stats, error)     { return repair.Stats{}, nil }
func (e *fakeEnv) GCNow(context.Context) (repair.Result, error)    { return repair.Result{}, nil }
func (e *fakeEnv) SetPolicy(ctx context.Context, k, m uint8) error {
	e.record(ctx, fmt.Sprintf("set-policy %d+%d", k, m))
	return nil
}

func (e *fakeEnv) SetState(ctx context.Context, n NodeRef, s meta.NodeState) error {
	e.record(ctx, "set-state "+n.Name+" "+s.String())
	return nil
}

func (e *fakeEnv) Remove(ctx context.Context, n NodeRef) error {
	e.record(ctx, "remove "+n.Name)
	return nil
}

func (e *fakeEnv) Fsck(context.Context, NodeRef) (bool, int, string, error) {
	return true, 0, "", nil
}

func (e *fakeEnv) Chunks(context.Context, NodeRef, string) ([]admin.ChunkReport, error) {
	return nil, nil
}
func (e *fakeEnv) FSMHash(context.Context, NodeRef) (uint64, string, error) { return 1, "h", nil }
func (e *fakeEnv) Goroutines(context.Context, NodeRef) (string, error)      { return e.dump, nil }
func (e *fakeEnv) Disk(context.Context, NodeRef) (DiskInfo, error)          { return DiskInfo{}, nil }

func (e *fakeEnv) Fill(ctx context.Context, n NodeRef, b uint64) error {
	e.record(ctx, fmt.Sprintf("fill %s %d", n.Name, b))
	return nil
}

func (e *fakeEnv) SetPolicyAuto(ctx context.Context) error {
	e.record(ctx, "set-policy auto")
	return nil
}

func (e *fakeEnv) Kill(ctx context.Context, n NodeRef) error {
	e.record(ctx, "kill "+n.Name)
	return nil
}

func (e *fakeEnv) Start(ctx context.Context, n NodeRef) error {
	e.record(ctx, "start "+n.Name)
	return nil
}

func (e *fakeEnv) Restart(ctx context.Context, ns []NodeRef) error {
	e.record(ctx, fmt.Sprintf("restart %d", len(ns)))
	return nil
}

func (e *fakeEnv) Partition(ctx context.Context, a, _ []NodeRef) error {
	e.record(ctx, fmt.Sprintf("partition %d", len(a)))
	return nil
}

func (e *fakeEnv) Heal(ctx context.Context) error {
	e.record(ctx, "heal")
	return nil
}

func (e *fakeEnv) Corrupt(ctx context.Context, n NodeRef, key string) error {
	e.record(ctx, "corrupt "+n.Name+" "+key)
	return nil
}

func (e *fakeEnv) FailDisk(ctx context.Context, d NodeRef) error {
	e.record(ctx, "fail-disk "+d.Name)
	return nil
}

func (e *fakeEnv) RestoreDisk(ctx context.Context, d NodeRef) error {
	e.record(ctx, "restore-disk "+d.Name)
	return nil
}

func (e *fakeEnv) AddDisk(ctx context.Context, h NodeRef) error {
	e.record(ctx, "add-disk "+h.Name)
	return nil
}

// fakeRun is a Run over e with the given phases and no periodic sampler.
func fakeRun(e *fakeEnv, out *bytes.Buffer, phases ...Phase) *Run {
	return &Run{Env: e, Profile: Profile{Name: "unit", ChunkSize: 4096}, Out: out, Phases: phases, Root: "/_soak/unit"}
}

func okPhase(name string) Phase {
	return Phase{Name: name, Run: func(_ context.Context, r *Run) error {
		r.check("I-1", "something held", true, "")
		return nil
	}}
}

// A phase that panics is one failed "phase completed" check with the driver's stack as a
// dump; the next phase still runs and the run returns a summary, not a dead driver.
//
// Invariant: I-111
func TestExecute_PhasePanicIsFailure(t *testing.T) {
	e := newFakeEnv(1)
	var out bytes.Buffer
	r := fakeRun(e, &out,
		Phase{Name: "boom", Run: func(context.Context, *Run) error {
			panic(errors.New("assignment to entry in nil map")) // the shape of the corpus-model bug
		}},
		okPhase("after"))
	sum, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Phases) != 2 || sum.Phases[0].Failed != 1 || sum.Phases[1].Failed != 0 || len(sum.Phases[1].Checks) != 1 {
		t.Fatalf("phases: %+v", sum.Phases)
	}
	chk := sum.Phases[0].Checks[0]
	if chk.Name != "phase completed" || !strings.Contains(chk.Text, "panic:") || !strings.Contains(chk.Text, "nil map") {
		t.Fatalf("panic check: %+v", chk)
	}
	dumps := 0
	for _, rec := range sum.Records {
		if rec.Kind == "dump" && rec.Node == "driver" && strings.Contains(rec.Text, "goroutine") {
			dumps++
		}
	}
	if dumps != 1 {
		t.Fatalf("%d driver dumps, want 1", dumps)
	}
	if sum.Failed != 1 || sum.Aborted != "" {
		t.Fatalf("failed %d aborted %q", sum.Failed, sum.Aborted)
	}
}

// A phase that runs past PhaseTimeout is failed with every node's goroutine dump, its
// context is cancelled and the run moves on.
//
// Invariant: I-111
func TestExecute_PhaseTimeoutIsFailure(t *testing.T) {
	e := newFakeEnv(2)
	var out bytes.Buffer
	released := make(chan struct{})
	r := fakeRun(e, &out,
		Phase{Name: "hang", Run: func(ctx context.Context, _ *Run) error {
			<-ctx.Done() // the phase context: cancelled by the timeout
			close(released)
			return ctx.Err()
		}},
		okPhase("after"))
	r.Profile.PhaseTimeout = 50 * time.Millisecond
	sum, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	default:
		t.Fatal("the hung phase was not cancelled")
	}
	if sum.Phases[0].Failed == 0 || !strings.Contains(sum.Phases[0].Checks[0].Text, "timed out") {
		t.Fatalf("timeout check: %+v", sum.Phases[0].Checks)
	}
	if len(sum.Phases) != 2 || sum.Phases[1].Failed != 0 {
		t.Fatalf("phases: %+v", sum.Phases)
	}
	dumps := map[string]bool{}
	for _, rec := range sum.Records {
		if rec.Kind == "dump" {
			dumps[rec.Node] = true
		}
	}
	if !dumps["n1"] || !dumps["n2"] {
		t.Fatalf("dumps from %v, want both nodes", dumps)
	}
}

// A phase that returns without recording a check and without skipping is a failure: a
// phase that asserted nothing has not passed. A skipped phase is not.
//
// Invariant: I-111
func TestExecute_SilentPhaseFails(t *testing.T) {
	e := newFakeEnv(1)
	var out bytes.Buffer
	r := fakeRun(e, &out,
		Phase{Name: "silent", Run: func(context.Context, *Run) error { return nil }},
		Phase{Name: "skipped", Run: func(_ context.Context, r *Run) error { r.skip("no capability"); return nil }})
	sum, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Phases[0].Failed != 1 || sum.Phases[0].Checks[0].Name != "phase recorded checks" {
		t.Fatalf("silent phase: %+v", sum.Phases[0])
	}
	if sum.Phases[1].Failed != 0 || !sum.Phases[1].Skipped || len(sum.Phases[1].Checks) != 0 {
		t.Fatalf("skipped phase: %+v", sum.Phases[1])
	}
}

// A run that cannot start (status fails) returns a summary with the reason and no
// phases, and the error; the report of that summary is ABORTED.
//
// Invariant: I-114
func TestExecute_AbortBeforePhases(t *testing.T) {
	e := newFakeEnv(1)
	e.statusErr = errors.New("connection refused")
	var out bytes.Buffer
	r := fakeRun(e, &out, okPhase("never"))
	sum, err := r.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err %v", err)
	}
	if sum.Aborted == "" || sum.Env != "fake" || len(sum.Phases) != 0 {
		t.Fatalf("summary: %+v", sum)
	}
	var html bytes.Buffer
	if err := Render(&html, sum); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html.String(), "ABORTED") || !strings.Contains(html.String(), "connection refused") {
		t.Fatal("report lacks the ABORTED verdict or the reason")
	}
}

// A run cancelled during a phase stops after it: the phases it never reached are NOT RUN,
// the summary says why, the run directory is still removed and Execute returns an error.
//
// Invariant: I-114
func TestExecute_CancelMarksNotRun(t *testing.T) {
	e := newFakeEnv(1)
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := fakeRun(e, &out,
		Phase{Name: "first", Run: func(_ context.Context, r *Run) error {
			r.check("I-1", "ran", true, "")
			cancel() // Ctrl-C lands here
			return nil
		}},
		okPhase("second"), okPhase("third"))
	sum, err := r.Execute(ctx)
	if err == nil || !strings.Contains(err.Error(), "cancelled after first") {
		t.Fatalf("err %v", err)
	}
	if len(sum.Phases) != 3 || sum.Phases[0].NotRun || !sum.Phases[1].NotRun || !sum.Phases[2].NotRun {
		t.Fatalf("phases: %+v", sum.Phases)
	}
	if sum.Phases[0].Failed != 0 || sum.Failed != 0 {
		t.Fatalf("a cancelled run is not a failed one: %+v", sum.Phases[0])
	}
	if _, err := e.fs.List(r.Root); err == nil {
		t.Fatalf("run directory %s survived the cancellation", r.Root)
	}
	var html bytes.Buffer
	if err := Render(&html, sum); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html.String(), "NOT RUN") || !strings.Contains(html.String(), "ABORTED") {
		t.Fatal("report lacks NOT RUN or ABORTED")
	}
}

// A phase interrupted after it killed a node still starts it again, on a context that
// outlives the cancellation.
//
// Invariant: I-112
func TestExecute_CancelledPhaseRestores(t *testing.T) {
	e := newFakeEnv(2)
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := fakeRun(e, &out, Phase{Name: "kill", Run: func(ctx context.Context, r *Run) error {
		u := r.undoer(ctx)
		defer u.run()
		n := r.nodes[1]
		if err := r.Env.Kill(ctx, n); err != nil {
			return err
		}
		u.add("start "+n.Name, func(c context.Context) error { return r.Env.Start(c, n) })
		r.check("I-1", "ran", true, "")
		cancel()
		return ctx.Err()
	}})
	if _, err := r.Execute(ctx); err == nil {
		t.Fatal("a cancelled run returns an error")
	}
	got := strings.Join(e.log(), "; ")
	if got != "kill n2 (live ctx); start n2 (live ctx)" {
		t.Fatalf("calls: %s", got)
	}
}

// The undo list runs the steps the phase did not get to, in reverse order, skips the ones
// marked done, and a failing step is noted while the rest still run.
//
// Invariant: I-112
func TestUndo_RunsRemainingStepsInReverse(t *testing.T) {
	var out bytes.Buffer
	r := &Run{Out: &out}
	var order []string
	u := r.undoer(context.Background())
	u.add("a", func(context.Context) error { order = append(order, "a"); return nil })
	u.add("b", func(context.Context) error { order = append(order, "b"); return nil })
	u.add("c", func(context.Context) error { order = append(order, "c"); return errors.New("refused") })
	u.add("d", func(context.Context) error { order = append(order, "d"); return nil })
	u.done("b")
	u.done("missing")
	u.run()
	if got := strings.Join(order, ","); got != "d,c,a" {
		t.Fatalf("order %s, want d,c,a", got)
	}
	restored, noted := 0, 0
	for _, rec := range r.records {
		switch {
		case rec.Kind == "event" && rec.Name == "restore":
			restored++
		case rec.Kind == "event" && rec.Name == "note" && strings.Contains(rec.Text, "restore c failed: refused"):
			noted++
		}
	}
	if restored != 2 || noted != 1 {
		t.Fatalf("%d restores, %d notes", restored, noted)
	}
	u.run() // idempotent: nothing left
	if len(order) != 3 {
		t.Fatal("a second run repeated steps")
	}
}

// Without Mutate (a live run without --destructive) the membership phase skips and the
// automation phase records its policy and budget checks skipped: no policy change, drain,
// fill or removal reaches the environment.
//
// Invariant: I-113
func TestPhases_MutatingStepsGated(t *testing.T) {
	e := newFakeEnv(3)
	e.caps = Caps{Corrupt: true, Kill: true, Partition: true, MultiGateway: true, Disks: true} // everything but Mutate
	var out bytes.Buffer
	r := fakeRun(e, &out, Phase{Name: "7 membership", Run: phaseMembership}, Phase{Name: "11 automation", Run: phaseAutomation})
	sum, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Phases[0].Skipped || !strings.Contains(sum.Phases[0].Reason, "--destructive") {
		t.Fatalf("membership: %+v", sum.Phases[0])
	}
	skipped := map[string]bool{}
	for _, c := range sum.Phases[1].Checks {
		if c.Skip {
			skipped[c.Cites] = true
		}
	}
	if !skipped["I-74"] || !skipped["I-75"] {
		t.Fatalf("automation skipped checks: %v", skipped)
	}
	for _, call := range e.log() {
		for _, bad := range []string{"set-policy", "set-state", "fill", "remove"} {
			if strings.HasPrefix(call, bad) {
				t.Fatalf("mutating call without Mutate: %s", call)
			}
		}
	}
}

// Goroutine dumps are capped per node and in total across the run: the node past the
// total is noted, not recorded.
func TestCaptureDumps_TotalCap(t *testing.T) {
	e := newFakeEnv(9)
	e.dump = strings.Repeat("g", dumpMax+10)
	var out bytes.Buffer
	r := fakeRun(e, &out)
	r.nodes = e.nodes
	r.captureDumps(context.Background())
	dumps, notes, bytesHeld := 0, 0, 0
	for _, rec := range r.records {
		switch {
		case rec.Kind == "dump":
			dumps++
			bytesHeld += len(rec.Text)
			if !strings.HasSuffix(rec.Text, "(truncated)") {
				t.Fatal("an over-long dump was not truncated")
			}
		case rec.Kind == "event" && strings.Contains(rec.Text, "omitted"):
			notes++
		}
	}
	want := dumpTotalMax / (dumpMax + len("\n... (truncated)")) // 7: the eighth would cross the total
	if dumps != want || notes != 9-dumps || bytesHeld > dumpTotalMax {
		t.Fatalf("%d dumps (want %d), %d omitted, %d bytes", dumps, want, notes, bytesHeld)
	}
}
