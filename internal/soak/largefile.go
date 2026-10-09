package soak

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
)

// --- 12 large file ---------------------------------------------------------------------------
//
// One file many times the WAL cap, written through the gateway in pieces and hashed as it
// goes, so the driver never holds it (docs/09 §6): the write path streams (docs/04 §9),
// a client that outruns the flusher is throttled rather than failed (I-79), the WAL and
// the flush buffers stay within their bounds and the heap stays flat (I-78, I-80), the
// bytes come back exact, and afterwards nothing is left behind.

const (
	largePiece   = 1 << 20 // one WriteAt
	largeSample  = 16      // status is sampled every this many pieces
	largeHeapGap = 256 << 20
)

func phaseLargeFile(ctx context.Context, r *Run) error {
	size := r.Profile.LargeFile
	if size <= 0 {
		r.skip("the profile sets no large file size")
		return nil
	}
	gw := r.gateways()[0]
	v, _ := r.volume(gw)
	p := join(r.Root, "large.bin")
	base, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	var baseHeap uint64
	if base.Process != nil {
		baseHeap = base.Process.HeapInuse
	}
	// orphans before the phase belong to earlier phases (migrations, restarts) and go
	// with the hourly sweep; the large file must add none
	_, orphans0, _, _ := r.Env.Fsck(ctx, gw)
	h := sha256.New()
	var peak struct {
		heap, flush, plan  uint64
		wal                int64
		walOver, flushOver int // samples past the bound
	}
	bounds := base.Gateway
	var writeErr error
	pieces := 0
	t := time.Now()
	for off := 0; off < size; off += largePiece {
		piece := r.randomBytes(min(largePiece, size-off))
		if err := v.WriteAt(p, int64(off), piece); err != nil {
			writeErr = fmt.Errorf("write at %d: %w", off, err)
			break
		}
		h.Write(piece)
		pieces++
		// the harness has no flusher goroutine: its Wait flushes synchronously, so a
		// client that writes this fast is drained between pieces as the run loop would;
		// a live rig's Wait of zero does nothing and its WRITEs wait at the cap (I-79)
		if err := r.Env.Wait(ctx, 0); err != nil {
			return err
		}
		if pieces%largeSample != 0 {
			continue
		}
		rep, err := r.Env.Status(ctx, gw)
		if err != nil {
			continue
		}
		if rep.Process != nil {
			peak.heap = max(peak.heap, rep.Process.HeapInuse)
		}
		if g := rep.Gateway; g != nil {
			// the flusher's own peak since start, not the sampled instant: a sample lands
			// between chunks as often as on one
			peak.wal, peak.flush, peak.plan = max(peak.wal, g.WALBytes), max(peak.flush, g.FlushPeak), max(peak.plan, g.PlanBytes)
			if bounds.WALCap > 0 && uint64(g.WALBytes) > uint64(g.DirtyInodes)*(bounds.WALCap+uint64(r.Profile.ChunkSize)+64) {
				peak.walOver++
			}
			if g.FlushCap > 0 && g.FlushPeak > g.FlushCap {
				peak.flushOver++
			}
		}
	}
	took := time.Since(t)
	mb := float64(size) / (1 << 20)
	r.event("large file written", fmt.Sprintf("%.0f MiB in %s (%.1f MiB/s) via %s", mb, took.Round(time.Millisecond), mb/took.Seconds(), gw.Name))
	r.checkAt("I-79", "every write acknowledged while outrunning the flusher", writeErr == nil, fmt.Sprintf("%d pieces of %s; %v", pieces, humanBytes(largePiece), writeErr), p, "", gw.Name)
	r.checkAt("I-80", "the WAL stayed within its cap", peak.walOver == 0, fmt.Sprintf("deepest WAL %s against a cap of %s; %d samples over", humanBytes(float64(peak.wal)), humanBytes(float64(bounds.WALCap)), peak.walOver), p, "", gw.Name)
	if baseHeap > 0 {
		budget := baseHeap + bounds.FlushCap + largeHeapGap
		r.checkAt("I-78", "the heap stayed flat", peak.heap <= budget, fmt.Sprintf("peak heap in use %s, baseline %s, budget %s", humanBytes(float64(peak.heap)), humanBytes(float64(baseHeap)), humanBytes(float64(budget))), p, "", gw.Name)
	}
	if writeErr != nil {
		return nil
	}
	// let the idle flush commit the tail, then the overlay must be empty
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	var rep admin.Report
	for i := 0; i < 20; i++ {
		if rep, err = r.Env.Status(ctx, gw); err == nil && rep.Gateway != nil && rep.Gateway.DirtyInodes == 0 {
			break
		}
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
	}
	drained := err == nil && rep.Gateway != nil && rep.Gateway.DirtyInodes == 0 && rep.Gateway.WALBytes == 0
	r.checkAt("I-80 L-13", "overlay and WAL empty after the idle flush", drained, fmt.Sprintf("%+v", rep.Gateway), p, "", gw.Name)
	if g := rep.Gateway; g != nil { // the flush of the tail counts too
		peak.flush = max(peak.flush, g.FlushPeak)
		if g.FlushCap > 0 && g.FlushPeak > g.FlushCap {
			peak.flushOver++
		}
	}
	r.checkAt("I-78", "flush buffers stayed within the bound", peak.flushOver == 0, fmt.Sprintf("peak %s against %s (plan bytes up to %s); %d samples over", humanBytes(float64(peak.flush)), humanBytes(float64(bounds.FlushCap)), humanBytes(float64(peak.plan)), peak.flushOver), p, "", gw.Name)
	want := [32]byte(h.Sum(nil))
	sum, n, err := v.Hash(p)
	r.checkAt("I-7 I-34", "content hash exact on the writing gateway", err == nil && n == int64(size) && sum == want, fmt.Sprintf("%d bytes, %v", n, err), p, "", gw.Name)
	if gws := r.gateways(); len(gws) > 1 {
		other, _ := r.volume(gws[1])
		sum, n, err := other.Hash(p)
		r.checkAt("I-12", "content hash exact on another gateway", err == nil && n == int64(size) && sum == want, fmt.Sprintf("%d bytes via %s, %v", n, gws[1].Name, err), p, "", gws[1].Name)
	}
	names, err := v.List(r.Root)
	temps := 0
	for _, name := range names {
		if strings.HasSuffix(name, ".tailraid-upload") || strings.HasPrefix(name, ".tmp-") {
			temps++
		}
	}
	r.check("I-81", "no temp file left in the run's directory", err == nil && temps == 0, fmt.Sprintf("%d temps among %d entries, %v", temps, len(names), err))
	ok, orphans1, detail, err := r.Env.Fsck(ctx, gw)
	r.checkAt("I-44 I-38", "fsck clean and no new orphans after the large file", err == nil && ok && orphans1 <= orphans0,
		fmt.Sprintf("%s; %d orphans before the phase%v", detail, orphans0, errText(err)), "", "", gw.Name)
	return nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return "; " + err.Error()
}
