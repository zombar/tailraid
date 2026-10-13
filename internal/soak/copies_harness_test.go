package soak_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"

	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/soak/smbclient"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
	"github.com/zombar/tailraid/internal/vfs"
)

// harnessCopyJob is a copy job on the in-process cluster: one goroutine per file, the
// outcome of each kept for Poll. As the compose worker does, a job that has reported its
// end is forgotten: a second Poll of it fails.
type harnessCopyJob struct {
	mu       sync.Mutex
	done     bool
	reported bool
	results  []soak.CopyResult
}

func (j *harnessCopyJob) Poll(context.Context) (bool, []soak.CopyResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.reported {
		return false, nil, errors.New("no such job: its end was already reported")
	}
	j.reported = j.done
	return j.done, append([]soak.CopyResult(nil), j.results...), nil
}

func (j *harnessCopyJob) run(files []soak.CopySpec, copyOne func(soak.CopySpec) error) {
	var wg sync.WaitGroup
	results := make([]soak.CopyResult, len(files))
	for i, f := range files {
		wg.Go(func() {
			results[i] = soak.CopyResult{Path: f.Path}
			if err := copyOne(f); err != nil {
				results[i].Err = err.Error()
				results[i].Exists = errors.Is(err, os.ErrExist)
			}
		})
	}
	wg.Wait()
	j.mu.Lock()
	j.done, j.results = true, results
	j.mu.Unlock()
}

const harnessSMBPassword = "soak-copies"

// harnessSMB is one node's SMB gateway on the in-process cluster.
type harnessSMB struct {
	fs   *vfs.FS
	addr string
	stop func()
}

// smbDial is how a client reaches node n's SMB gateway: one server per node over its
// current filesystem, started on first use and again after a restart replaced the
// filesystem, so two jobs of one node are two connections to one gateway.
func (e *harnessEnv) smbDial(n soak.NodeRef) smbclient.Dial {
	return func(ctx context.Context) (*smb2.Share, func(), error) {
		addr, err := e.smbAddr(n)
		if err != nil {
			return nil, nil, err
		}
		d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: smbgw.User, Password: harnessSMBPassword}}
		sess, err := d.Dial(ctx, addr)
		if err != nil {
			return nil, nil, err
		}
		sh, err := sess.Mount(smbgw.ShareName)
		if err != nil {
			_ = sess.Logoff()
			return nil, nil, err
		}
		return sh, func() { _ = sh.Umount(); _ = sess.Logoff() }, nil
	}
}

func (e *harnessEnv) smbAddr(n soak.NodeRef) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	nd := e.node(n)
	if s := e.smb[int(n.Idx)]; s != nil {
		if s.fs == nd.FS {
			return s.addr, nil
		}
		s.stop()
	}
	srv, err := smbgw.NewServer(nd.FS, nd.Node.FSM, harnessSMBPassword)
	if err != nil {
		return "", err
	}
	l, err := smbgw.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	sctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { _ = smbgw.Serve(sctx, l, srv); close(served) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-served }) }
	e.t.Cleanup(stop)
	if e.smb == nil {
		e.smb = map[int]*harnessSMB{}
	}
	e.smb[int(n.Idx)] = &harnessSMB{fs: nd.FS, addr: l.Addr().String(), stop: stop}
	return l.Addr().String(), nil
}

// stopSMB ends node n's SMB gateway and its connections, as the process's exit does; the
// next dial starts one on the node's new vfs. Callers hold e.mu.
func (e *harnessEnv) stopSMB(n soak.NodeRef) {
	if s := e.smb[int(n.Idx)]; s != nil {
		s.stop()
		delete(e.smb, int(n.Idx))
	}
}

// StartCopies copies over SMB (the node's gateway, one connection per CopySpec.Conn) or
// straight on the node's vfs (one handle per file, as the NFS gateway holds it).
func (e *harnessEnv) StartCopies(ctx context.Context, n soak.NodeRef, proto string, files []soak.CopySpec) (soak.CopyJob, error) {
	e.mu.RLock()
	fs := e.node(n).FS
	e.mu.RUnlock()
	job := &harnessCopyJob{}
	switch proto {
	case "nfs":
		go job.run(files, func(f soak.CopySpec) error {
			flag := os.O_RDWR | os.O_CREATE | os.O_EXCL
			if f.Mode == soak.CopyReplace {
				flag = os.O_RDWR | os.O_CREATE | os.O_TRUNC
			}
			h, err := fs.OpenFile(f.Path, flag, 0o644)
			if err != nil {
				return err
			}
			vf := h.(*vfs.File)
			err = smbclient.Pieces(f, vf.Truncate, vf.WriteAt)
			return errors.Join(err, vf.Close())
		})
	case "smb":
		if _, err := e.smbAddr(n); err != nil {
			return nil, err
		}
		go func() {
			res := smbclient.Copy(context.Background(), e.smbDial(n), files)
			job.mu.Lock()
			job.done, job.results = true, res
			job.mu.Unlock()
		}()
	default:
		return nil, errors.New("unknown protocol " + proto)
	}
	return job, nil
}

// HoldDeleteOnClose holds a delete-on-close handle open on node n's gateway.
func (e *harnessEnv) HoldDeleteOnClose(ctx context.Context, n soak.NodeRef, path string) (func(context.Context) error, error) {
	release, err := smbclient.Hold(ctx, e.smbDial(n), path)
	if err != nil {
		return nil, err
	}
	return func(context.Context) error { return release() }, nil
}

// SMBRaces runs phase 23's SMB handle races on node n's gateway.
func (e *harnessEnv) SMBRaces(ctx context.Context, n soak.NodeRef, dir string) ([]soak.RaceResult, error) {
	return smbclient.Races(ctx, e.smbDial(n), dir), nil
}

func (e *harnessEnv) SetCodecView(_ context.Context, observer, about soak.NodeRef, v uint8) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	e.node(observer).Prober.SetCodecView(about.Idx, v)
	return nil
}

func (e *harnessEnv) DelayPuts(_ context.Context, n soak.NodeRef, d time.Duration) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	e.node(n).Net.DelayPuts(d)
	return nil
}

// RestartGraceful flushes the node, as serve's shutdown does, and restarts it.
func (e *harnessEnv) RestartGraceful(ctx context.Context, n soak.NodeRef) error {
	e.mu.RLock()
	nd := e.node(n)
	if e.c.Alive(nd.I) {
		_ = nd.Flush.FlushAll(ctx)
	}
	e.mu.RUnlock()
	return e.Restart(ctx, []soak.NodeRef{n})
}

// RestartIdle is update's wait (I-221) on the harness: time passes until the node's
// overlay has held nothing dirty and its clients have written nothing for three steps
// in a row and for idleWall of the wall clock, then the graceful restart. The clients
// run on the wall clock: three steps of the fake one can pass before a copy has
// written its first byte.
func (e *harnessEnv) RestartIdle(ctx context.Context, n soak.NodeRef) error {
	const idleWall = 2 * time.Second
	deadline := time.Now().Add(2 * time.Minute)
	var last uint64
	quiet := 0
	var since time.Time // the wall time the current quiet run began
	for quiet < 3 || time.Since(since) < idleWall {
		if time.Now().After(deadline) {
			return errors.New("the gateway never went idle")
		}
		if err := e.Wait(ctx, time.Second); err != nil {
			return err
		}
		e.mu.RLock()
		nd := e.node(n)
		_, written := nd.FS.ClientBytes()
		dirty := len(nd.Ov.Dirty())
		e.mu.RUnlock()
		if dirty == 0 && written == last {
			if quiet == 0 {
				since = time.Now()
			}
			quiet++
		} else {
			quiet = 0
		}
		last = written
	}
	return e.RestartGraceful(ctx, n)
}

var (
	_ soak.CopyEnv = (*harnessEnv)(nil)
	_ soak.RaceEnv = (*harnessEnv)(nil)
)

// Phase 22 alone on the in-process cluster: Explorer-style copies over SMB and NFS on two
// gateways while each fault hits the rest, every copy whole afterwards (docs/09 §6).
// Invariant: I-217, I-219, I-221
func TestSoak_CopiesUnderFaultsAgainstHarness(t *testing.T) {
	// production's gateways retry a proposal until stale (L-187); the copies meet
	// elections in every round
	c := testharness.NewCluster(t, 4, 2, 1, testharness.WALCap(512<<10), testharness.RetryUntilStale())
	defer c.Close()
	env := &harnessEnv{t: t, c: c}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: soak.StandardPhases("22")}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Phases) != 1 {
		t.Fatalf("ran %d phases", len(sum.Phases))
	}
	ph := sum.Phases[0]
	if ph.Skipped {
		t.Fatalf("skipped: %s", ph.Reason)
	}
	for _, chk := range ph.Checks {
		if chk.OK == nil || !*chk.OK {
			t.Errorf("seed %d: %s: %s", seed, chk.Name, chk.Text)
		}
	}
	if len(ph.Checks) < 6*4 {
		t.Errorf("%d checks, want four per round", len(ph.Checks))
	}
}

// Phase 23 against the in-process cluster: every round's checks pass, the SMB handle
// races and the browser uploads included (the harness offers both), on a cluster whose
// gateways retry until stale as production's do.
// Invariant: I-222, I-223, I-224, I-225, I-226
func TestSoak_MultiUserAgainstHarness(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1, testharness.WALCap(512<<10), testharness.RetryUntilStale())
	defer c.Close()
	env := &harnessEnv{t: t, c: c}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: soak.StandardPhases("23")}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Phases) != 1 {
		t.Fatalf("ran %d phases", len(sum.Phases))
	}
	ph := sum.Phases[0]
	if ph.Skipped {
		t.Fatalf("skipped: %s", ph.Reason)
	}
	for _, chk := range ph.Checks {
		if chk.OK == nil {
			t.Errorf("seed %d: %s skipped: %s", seed, chk.Name, chk.Text)
		} else if !*chk.OK {
			t.Errorf("seed %d: %s: %s", seed, chk.Name, chk.Text)
		}
	}
	if len(ph.Checks) < 20 {
		t.Errorf("%d checks, want at least two per round", len(ph.Checks))
	}
}

// A restart drops the node's SMB connections, as a stopped process does: a client
// writing through one hears an error instead of writing into the old node's overlay,
// which no flusher drains (phase 22.6 lost four acknowledged copies so on CI).
// Regression: TestSoak_CopiesUnderFaultsAgainstHarness 22.6, run 38407158867
func TestHarnessEnv_RestartDropsSMBConnections(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	env := &harnessEnv{t: t, c: c}
	n := soak.NodeRef{Idx: c.Nodes[1].Idx(), Name: "n1", RPC: c.Nodes[1].RPCAddr()}
	sh, end, err := env.smbDial(n)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	f, err := sh.OpenFile("restart.bin", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("before"), 4096); err != nil {
		t.Fatal(err)
	}
	if err := env.Restart(t.Context(), []soak.NodeRef{n}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("after"), 8192); err == nil {
		t.Fatal("a write through a connection of the node before its restart succeeded")
	}
}
