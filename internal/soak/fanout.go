package soak

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/repair"
)

// --- 18 repair fan-out ---------------------------------------------------------------------
//
// The repair pass as a background operation (docs/06 §4, I-174, I-175): a change to the
// neighbouring policy makes every chunk stale, the pass that migrates them runs with its
// workers in flight while a file is read cold through another gateway, and the leader's
// status is sampled throughout. Reported: the pass rate, the in-flight peak and the cold
// read's throughput during the pass against the same read idle, measured just before.
// Checked: repairs in flight never above repair.workers, the pass overlapped chunks, and
// the read during the pass kept at least a third of its idle throughput. The policy is
// restored on the way out and the migration back completes. Needs Caps.Mutate (a live
// cluster: --destructive).

const fanoutChunks = 64

func phaseFanOut(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Mutate {
		r.skip("the backlog is a policy change: --destructive on a live cluster")
		return nil
	}
	gws := r.gateways()
	if len(gws) == 0 {
		r.skip("no gateway reachable")
		return nil
	}
	writer, reader := gws[0], gws[len(gws)-1]
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	k0, m0 := rep.Volume.K, rep.Volume.M
	if k0 <= 1 {
		r.skip(fmt.Sprintf("no neighbouring policy at %d+%d", k0, m0))
		return nil
	}
	k1, m1 := k0-1, m0+1
	p := join(r.Root, "fanout.bin")
	size := fanoutChunks * r.Profile.ChunkSize
	want, ok, err := r.writeCommitted(ctx, writer, p, size)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	rv := r.volume(reader)
	idle, ok := r.coldRead(rv, reader, p, size, want, "idle")
	if !ok {
		return nil
	}
	// the backlog: every chunk stale under the neighbouring policy, restored on the way out
	wasAuto := rep.Volume.Auto
	restore := func(c context.Context) error {
		if wasAuto {
			return r.Env.SetPolicyAuto(c)
		}
		return r.Env.SetPolicy(c, k0, m0)
	}
	u := r.undoer(ctx)
	defer u.run()
	if err := r.Env.SetPolicy(ctx, k1, m1); err != nil {
		r.check("L-100", fmt.Sprintf("set-redundancy %d+%d accepted", k1, m1), false, err.Error())
		return nil
	}
	u.add("set-redundancy back", restore)
	// the leader's own pass may start on the backlog before the one asked for here and
	// hold the scan lock until it ends; the stale count before and after shows how much
	// each did
	staleBefore := -1
	if st, _, err := r.anyStatus(ctx); err == nil {
		staleBefore = st.Volume.StalePolicy
	}
	r.event("set-redundancy", fmt.Sprintf("%d+%d: %d chunks stale", k1, m1, staleBefore))

	type passResult struct {
		st   repair.Stats
		took time.Duration
		err  error
	}
	done := make(chan passResult, 1)
	go func() {
		t := time.Now()
		st, err := r.repairNow(ctx)
		done <- passResult{st, time.Since(t), err}
	}()
	// sample the leader while the pass runs: the in-flight peak against its workers
	leader := r.leaderRef(ctx)
	var (
		mu                     sync.Mutex
		samples, peak, workers int
		seen                   = make(chan struct{})
		seenOnce               sync.Once
	)
	sctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-tick.C:
				st, err := r.Env.Status(sctx, leader)
				if err != nil || st.Repair == nil || st.Repair.InProgress == nil {
					continue
				}
				pr := st.Repair.InProgress
				mu.Lock()
				samples++
				peak = max(peak, pr.InFlight)
				workers = pr.Workers
				mu.Unlock()
				seenOnce.Do(func() { close(seen) })
			}
		}
	})
	// the cold read during the pass, once a sample has seen the pass running
	var res passResult
	running, finished := false, false
	select {
	case <-seen:
		running = true
	case res = <-done:
		finished = true
	case <-ctx.Done():
		stop()
		wg.Wait()
		return ctx.Err()
	}
	during, readOK := 0.0, false
	if running {
		during, readOK = r.coldRead(rv, reader, p, size, want, "during the pass")
	}
	if !finished {
		select {
		case res = <-done:
		case <-ctx.Done():
			stop()
			wg.Wait()
			return ctx.Err()
		}
	}
	stop()
	wg.Wait()
	mu.Lock()
	s, pk, w := samples, peak, workers
	mu.Unlock()
	if res.err != nil {
		r.check("I-174", "the migration pass completed", false, res.err.Error())
		return nil
	}
	rate := float64(res.st.Scanned) / max(res.took.Seconds(), 1e-9)
	staleAfter := -1
	if st, _, err := r.anyStatus(ctx); err == nil {
		staleAfter = st.Volume.StalePolicy
	}
	r.event("repair pass", fmt.Sprintf("%d chunks scanned, %d migrated (%d deferred, %d partial, %d errors) in %s (%.1f chunks/s), %d in flight at peak over %d samples; %d stale before, %d after (the leader's own pass may have taken part)",
		res.st.Scanned, res.st.Migrated, res.st.Deferred, res.st.Partial, res.st.Errors, res.took.Round(time.Millisecond), rate, pk, s, staleBefore, staleAfter))
	const bound, overlap, invisible = "repairs in flight within repair.workers", "the pass ran chunks concurrently", "a cold read during the pass keeps a third of its idle throughput"
	if s == 0 {
		r.checkSkipped("I-174", bound, "the pass ended before a status sample saw it running")
		r.checkSkipped("I-174", overlap, "the pass ended before a status sample saw it running")
	} else {
		r.check("I-174", bound, pk <= w, fmt.Sprintf("peak %d of %d workers over %d samples", pk, w, s))
		r.check("I-174", overlap, pk >= 2 || w < 2, fmt.Sprintf("peak %d in flight, %d workers", pk, w))
	}
	switch {
	case !running:
		r.checkSkipped("I-175", invisible, "the pass ended before the read could start")
	case len(gws) == 1:
		// the same gateway read the file a moment ago: its own cache serves the second
		// read, so the figure says nothing about the pass (as phase 17's readahead case)
		r.checkSkipped("I-175", invisible, fmt.Sprintf("one gateway: the client's own cache served the read it had just done (%.1f MiB/s against %.1f MiB/s idle)", during, idle))
	case readOK:
		r.check("I-175", invisible, during >= idle/3, fmt.Sprintf("%.1f MiB/s during the pass, %.1f MiB/s idle, via %s", during, idle, reader.Name))
	}
	// back to the policy the cluster had, migrated again
	if err := restore(ctx); err != nil {
		r.check("L-100", fmt.Sprintf("set-redundancy %d+%d back accepted", k0, m0), false, err.Error())
		return nil
	}
	u.done("set-redundancy back")
	after, err := r.repairUntilClean(ctx)
	r.check("I-15 I-32 L-173", fmt.Sprintf("migration back to %d+%d complete", k0, m0), err == nil && after.Volume.StalePolicy == 0 && after.Volume.Degraded == 0,
		fmt.Sprintf("%d stale, %d degraded, %v", after.Volume.StalePolicy, after.Volume.Degraded, err))
	r.coldRead(rv, reader, p, size, want, "after the migration back")
	if err := r.volume(writer).Remove(p); err != nil {
		r.checkAt("I-81", "fan-out file removed", false, err.Error(), p, "", writer.Name)
	}
	return nil
}

// coldRead hashes p whole through v, reports its throughput as an event and checks the
// content against want; ok is the content check.
func (r *Run) coldRead(v Volume, node NodeRef, p string, size int, want [32]byte, when string) (mbps float64, ok bool) {
	t := time.Now()
	sum, n, err := v.Hash(p)
	took := time.Since(t)
	mb := float64(size) / (1 << 20)
	mbps = mb / max(took.Seconds(), 1e-9)
	r.event("cold read "+when, fmt.Sprintf("%.0f MiB in %s (%.1f MiB/s) via %s", mb, took.Round(time.Millisecond), mbps, node.Name))
	ok = err == nil && n == int64(size) && sum == want
	r.checkAt("I-12 I-40", "content hash exact on the cold read "+when, ok, fmt.Sprintf("%d bytes via %s, %v", n, node.Name, err), p, "", node.Name)
	return mbps, ok
}
