package soak

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/vfs"
)

// --- 33 stage ----------------------------------------------------------------------------
//
// A copy staged ahead of the cluster (docs/04 §3, §2.1, docs/09 §6): the gateway reports
// its stage, takes a file written as fast as the driver can at local speed, its log keeps
// about the staged bytes while the flusher drains it front to back, status reports what
// is staged and how fast it drains, and the stage empties with the file exact on another
// gateway.

// stageFileMax bounds the phase's file: big enough to cross several segments, small
// enough for the quick profile.
const stageFileMax = 512 << 20

func phaseStage(ctx context.Context, r *Run) error {
	gw := r.gateways()[0]
	base, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	if base.Gateway == nil || base.Gateway.Stage == 0 {
		r.skip("the gateway reports no stage (status without the stage fields)")
		return nil
	}
	stage := base.Gateway.Stage
	r.check("I-396", "the gateway reports its stage, at least 1 GiB", stage >= vfs.MinStage,
		fmt.Sprintf("stage %s, WAL cap %s on %s", humanBytes(float64(stage)), humanBytes(float64(base.Gateway.WALCap)), gw.Name))
	size := min(r.Profile.LargeFile, stageFileMax, int(stage/2)) //nolint:gosec // stage/2 of a real disk
	if size <= 0 {
		r.skip("the profile sets no large file size")
		return nil
	}
	flushed0 := base.Gateway.FlushedBytes
	v := r.volume(gw)
	p := join(r.Root, "staged.bin")
	h := sha256.New()
	var peak struct {
		staged, wal uint64
		rate        uint64
		walOver     int
		mismatch    int
		samples     int
		over        []string
		warnWanted  int // samples with more than StageWarnAt staged
		warnMissing int // ... whose report carried no staged warning
	}
	sample := func(when string) {
		rep, err := r.Env.Status(ctx, gw)
		if err != nil || rep.Gateway == nil {
			return
		}
		g := rep.Gateway
		peak.samples++
		peak.staged, peak.rate = max(peak.staged, g.Staged), max(peak.rate, g.DrainRate)
		peak.wal = max(peak.wal, uint64(max(g.WALBytes, 0)))
		if g.Staged != g.DirtyBytes+g.PlanBytes {
			peak.mismatch++
		}
		if g.Staged > admin.StageWarnAt {
			peak.warnWanted++
			if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.HasPrefix(w, "staged ") }) {
				peak.warnMissing++
			}
		}
		// a log drained front to back sheds its segments: about the staged bytes plus two
		// segments and a chunk per dirty inode (I-397)
		slack := uint64(g.DirtyInodes)*(2*vfs.SegmentSize+uint64(r.Profile.ChunkSize)) + 1<<20
		if uint64(max(g.WALBytes, 0)) > g.Staged+slack {
			peak.walOver++
			if len(peak.over) < 3 {
				peak.over = append(peak.over, fmt.Sprintf("%s: WAL %s for %s staged", when, humanBytes(float64(g.WALBytes)), humanBytes(float64(g.Staged))))
			}
		}
	}
	var writeErr error
	t := time.Now()
	for off, n := 0, 0; off < size; off, n = off+largePiece, n+1 {
		piece := r.randomBytes(min(largePiece, size-off))
		if err := v.WriteAt(p, int64(off), piece); err != nil {
			writeErr = fmt.Errorf("write at %d: %w", off, err)
			break
		}
		h.Write(piece)
		// the harness has no flusher goroutine: its Wait flushes synchronously, as the run
		// loop would; a live rig's Wait of zero does nothing (see phase 12)
		if err := r.Env.Wait(ctx, 0); err != nil {
			return err
		}
		if n%largeSample == largeSample-1 {
			sample(fmt.Sprintf("at %s", humanBytes(float64(off))))
		}
	}
	took := time.Since(t)
	mb := float64(size) / (1 << 20)
	r.event("staged file written", fmt.Sprintf("%.0f MiB in %s (%.1f MiB/s) via %s, peak staged %s", mb, took.Round(time.Millisecond), mb/took.Seconds(), gw.Name, humanBytes(float64(peak.staged))))
	r.checkAt("I-396 I-79", "every write of the staged file acknowledged", writeErr == nil, fmt.Sprintf("%s in %s; %v", humanBytes(float64(size)), took.Round(time.Millisecond), writeErr), p, "", gw.Name)
	if writeErr != nil {
		return nil //nolint:nilerr // the failed check carries the error; the phase itself completed
	}
	// the stage drains: the idle flush commits the tail, then nothing is staged
	var rep admin.Report
	for range 40 {
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
		sample("draining")
		if rep, err = r.Env.Status(ctx, gw); err == nil && rep.Gateway != nil && rep.Gateway.Staged == 0 && rep.Gateway.WALBytes == 0 {
			break
		}
	}
	g := rep.Gateway
	drained := g != nil && g.Staged == 0 && g.WALBytes == 0
	r.checkAt("I-397", "the stage drains and the log goes with it", drained, fmt.Sprintf("%+v", g), p, "", gw.Name)
	switch peak.samples {
	case 0:
		r.checkSkipped("I-397", "the log keeps about the staged bytes", "no status sample landed")
		r.checkSkipped("I-398", "staged is the dirty and plan bytes", "no status sample landed")
	default:
		r.checkAt("I-397", "the log keeps about the staged bytes", peak.walOver == 0,
			fmt.Sprintf("deepest WAL %s, peak staged %s; %d of %d samples over %s", humanBytes(float64(peak.wal)), humanBytes(float64(peak.staged)), peak.walOver, peak.samples, strings.Join(peak.over, "; ")), p, "", gw.Name)
		r.check("I-398", "staged is the dirty and plan bytes", peak.mismatch == 0, fmt.Sprintf("%d of %d samples differ", peak.mismatch, peak.samples))
	}
	if g != nil {
		r.check("I-398", "the flusher's committed bytes count the file", g.FlushedBytes >= flushed0+uint64(size), //nolint:gosec // size > 0
			fmt.Sprintf("flushed_bytes %s → %s for a %s file", humanBytes(float64(flushed0)), humanBytes(float64(g.FlushedBytes)), humanBytes(float64(size))))
	}
	if peak.rate > 0 {
		r.check("I-398", "status reports a drain rate while the stage drains", true, fmt.Sprintf("peak %s/s", humanBytes(float64(peak.rate))))
	} else {
		r.checkSkipped("I-398", "status reports a drain rate while the stage drains", "no sample caught one: the stage drained between samples, or the environment runs no flusher loop")
	}
	if peak.warnWanted > 0 {
		r.check("I-398", "status warns while more than 1 GiB is staged", peak.warnMissing == 0, fmt.Sprintf("%d of %d samples over 1 GiB without the warning", peak.warnMissing, peak.warnWanted))
	} else {
		r.checkSkipped("I-398", "status warns while more than 1 GiB is staged", fmt.Sprintf("no sample held more: the stage peaked at %s", humanBytes(float64(peak.staged))))
	}
	want := [32]byte(h.Sum(nil))
	sum, n, err := v.Hash(p)
	r.checkAt("I-7 I-34", "the staged file exact on the writing gateway", err == nil && n == int64(size) && sum == want, fmt.Sprintf("%d bytes, %v", n, err), p, "", gw.Name)
	if gws := r.gateways(); len(gws) > 1 {
		sum, n, err := r.volume(gws[1]).Hash(p)
		r.checkAt("I-12", "the staged file exact on another gateway", err == nil && n == int64(size) && sum == want, fmt.Sprintf("%d bytes via %s, %v", n, gws[1].Name, err), p, "", gws[1].Name)
	}
	if err := v.Remove(p); err != nil {
		r.event("staged file left", err.Error())
	}
	return nil
}
