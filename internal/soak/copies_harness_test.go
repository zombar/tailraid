package soak_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"

	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
	"github.com/zombar/tailraid/internal/vfs"
)

// harnessCopyJob is a copy job on the in-process cluster: one goroutine per file, the
// outcome of each kept for Poll.
type harnessCopyJob struct {
	mu      sync.Mutex
	done    bool
	results []soak.CopyResult
}

func (j *harnessCopyJob) Poll(context.Context) (bool, []soak.CopyResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done, append([]soak.CopyResult(nil), j.results...), nil
}

func (j *harnessCopyJob) run(files []soak.CopySpec, copyOne func(soak.CopySpec) error, after func()) {
	var wg sync.WaitGroup
	results := make([]soak.CopyResult, len(files))
	for i, f := range files {
		wg.Go(func() {
			results[i] = soak.CopyResult{Path: f.Path}
			if err := copyOne(f); err != nil {
				results[i].Err = err.Error()
			}
		})
	}
	wg.Wait()
	if after != nil {
		after()
	}
	j.mu.Lock()
	j.done, j.results = true, results
	j.mu.Unlock()
}

// copyPieces is the copy engine's sequence on an open handle: the final length first,
// then the bytes front to back in CopyPiece writes.
func copyPieces(f soak.CopySpec, truncate func(int64) error, writeAt func([]byte, int64) (int, error)) error {
	if err := truncate(int64(f.Size)); err != nil {
		return err
	}
	data := soak.CopyPayload(f.Size, f.Seed)
	for off := 0; off < len(data); off += soak.CopyPiece {
		if _, err := writeAt(data[off:min(off+soak.CopyPiece, len(data))], int64(off)); err != nil {
			return err
		}
	}
	return nil
}

const harnessSMBPassword = "soak-copies"

// StartCopies copies over SMB (a gateway server on the node's filesystem, one go-smb2
// connection for every file) or straight on the node's vfs (one handle per file, as the
// NFS gateway holds it).
func (e *harnessEnv) StartCopies(ctx context.Context, n soak.NodeRef, proto string, files []soak.CopySpec) (soak.CopyJob, error) {
	e.mu.RLock()
	nd := e.node(n)
	fs, fsm := nd.FS, nd.Node.FSM
	e.mu.RUnlock()
	job := &harnessCopyJob{}
	switch proto {
	case "nfs":
		go job.run(files, func(f soak.CopySpec) error {
			h, err := fs.OpenFile(f.Path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			vf := h.(*vfs.File)
			err = copyPieces(f, vf.Truncate, vf.WriteAt)
			return errors.Join(err, vf.Close())
		}, nil)
	case "smb":
		srv, err := smbgw.NewServer(fs, fsm, harnessSMBPassword)
		if err != nil {
			return nil, err
		}
		l, err := smbgw.Listen(ctx, "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		sctx, cancel := context.WithCancel(context.Background())
		served := make(chan struct{})
		go func() { _ = smbgw.Serve(sctx, l, srv); close(served) }()
		d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: smbgw.User, Password: harnessSMBPassword}}
		sess, err := d.Dial(ctx, l.Addr().String())
		if err != nil {
			cancel()
			<-served
			return nil, err
		}
		sh, err := sess.Mount(smbgw.ShareName)
		if err != nil {
			_ = sess.Logoff()
			cancel()
			<-served
			return nil, err
		}
		go job.run(files, func(f soak.CopySpec) error {
			h, err := sh.OpenFile(strings.TrimPrefix(f.Path, "/"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			err = copyPieces(f, h.Truncate, h.WriteAt)
			return errors.Join(err, h.Close())
		}, func() {
			_ = sh.Umount()
			_ = sess.Logoff()
			cancel()
			<-served
		})
	default:
		return nil, errors.New("unknown protocol " + proto)
	}
	return job, nil
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
// in a row, then the graceful restart.
func (e *harnessEnv) RestartIdle(ctx context.Context, n soak.NodeRef) error {
	deadline := time.Now().Add(2 * time.Minute)
	var last uint64
	quiet := 0
	for quiet < 3 {
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
			quiet++
		} else {
			quiet = 0
		}
		last = written
	}
	return e.RestartGraceful(ctx, n)
}

var _ soak.CopyEnv = (*harnessEnv)(nil)

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
