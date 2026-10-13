package soak

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chart"
)

// The data-slot spread check (I-33) judges a target only once it expects minJudged data
// slots (a Poisson count below that is too noisy to call), within spreadSigmas standard
// deviations: with a dozen targets per run, 3σ would flag one run in twenty by chance.
const (
	minJudged    = 10
	spreadSigmas = 3.5
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
	v := r.volume(gw)
	p := join(r.Root, "large.bin")
	base, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	var baseHeap uint64
	samples := 0 // status samples that landed during the write
	if base.Process != nil {
		baseHeap = base.Process.HeapInuse
	}
	// orphans before the phase belong to earlier phases (migrations, restarts) and go
	// with the hourly sweep; the large file must add none. A previous phase's restore may
	// have just moved the automatic policy (phase 11's budgets), and a migration still
	// running under the phase would leave each moved chunk's old shard here as an orphan
	// of its own, so the migration finishes first.
	if _, err := r.repairUntilClean(ctx); err != nil {
		return err
	}
	fsck0, _ := r.Env.Fsck(ctx, gw)
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
		samples++
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
	switch {
	case bounds.WALCap == 0:
		r.checkSkipped("I-80", "the WAL stayed within its cap", "the gateway reports no WAL cap (status without the gateway section)")
	case samples == 0:
		r.checkSkipped("I-80", "the WAL stayed within its cap", "no status sample landed during the write")
	default:
		r.checkAt("I-80", "the WAL stayed within its cap", peak.walOver == 0, fmt.Sprintf("deepest WAL %s against a cap of %s; %d samples over %d", humanBytes(float64(peak.wal)), humanBytes(float64(bounds.WALCap)), peak.walOver, samples), p, "", gw.Name)
	}
	if baseHeap > 0 && samples > 0 {
		budget := baseHeap + bounds.FlushCap + largeHeapGap
		r.checkAt("I-78", "the heap stayed flat", peak.heap <= budget, fmt.Sprintf("peak heap in use %s, baseline %s, budget %s", humanBytes(float64(peak.heap)), humanBytes(float64(baseHeap)), humanBytes(float64(budget))), p, "", gw.Name)
	} else {
		r.checkSkipped("I-78", "the heap stayed flat", "no process vitals from the gateway (status without the process section, or no sample during the write)")
	}
	if writeErr != nil {
		return nil //nolint:nilerr // the failed check above carries the error; the phase itself completed
	}
	// let the idle flush commit the tail, then the overlay must be empty
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	var rep admin.Report
	for range 20 {
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
		other := r.volume(gws[1])
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
	// a new orphan passes only when its chunk's history explains it: the write path leaves
	// a few for the sweep (a PUT cut after k acks that landed anyway, an upload whose
	// commit did not apply, L-190), and other gateways write meanwhile; one nothing
	// explains is a leak. A list fsck capped compares by count instead.
	fsck1, err := r.Env.Fsck(ctx, gw)
	explained, unexplained := judgeNewOrphans(fsck0.Listed, fsck1.Listed, r.chunkHistory(ctx))
	capped := fsck0.Orphans > len(fsck0.Listed) || fsck1.Orphans > len(fsck1.Listed)
	clean := len(unexplained) == 0 && (!capped || fsck1.Orphans <= fsck0.Orphans)
	detail := fmt.Sprintf("%s; %d orphans before the phase; %d new explained, %d not", fsck1.Detail, fsck0.Orphans, len(explained), len(unexplained))
	if len(unexplained) > 0 {
		detail += "; unexplained: " + orphanText(unexplained, 8)
	}
	if len(explained) > 0 {
		detail += "; explained: " + orphanText(explained, 8)
	}
	r.checkAt("I-44 I-38", "fsck clean and every new orphan explained after the large file", err == nil && fsck1.OK && clean,
		detail+errText(err), "", "", gw.Name)
	r.placementChecks(ctx, p, size, rep)
	return nil
}

// placementEvent carries the large file's per-node slot counts to the report's figure.
const placementEvent = "large file placement"

// placementChecks: where the large file's chunks landed (docs/09 §6): every chunk fully
// placed (I-9), its data slots on voters (I-77), data slots spread among the voters in
// proportion to capacity within the statistical margin (I-33), and the figure.
func (r *Run) placementChecks(ctx context.Context, p string, size int, rep admin.Report) {
	reps, err := r.chunksOf(ctx, p)
	if err != nil {
		r.check("I-9", "chunk reports of the large file", false, err.Error())
		return
	}
	type nodeInfo struct {
		name  string
		cap   uint64
		voter bool
		live  bool
	}
	nodes := map[int]nodeInfo{}
	var capAll, capVoters uint64
	liveVoters := 0
	vt := voterTargets(rep) // per target: a disk of a voter is a voter target (docs/14 §5)
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if n.State != "active" {
			continue
		}
		nodes[int(n.Idx)] = nodeInfo{n.Name, n.Capacity, vt[int(n.Idx)], n.Live}
		capAll += n.Capacity
		if vt[int(n.Idx)] {
			capVoters += n.Capacity
			if n.Live {
				liveVoters++
			}
		}
	}
	data, parity := map[int]int{}, map[int]int{}
	missing, dataOffVoters, k := 0, 0, 0
	for i := range reps {
		c := &reps[i]
		k = int(c.K)
		for j, s := range c.Slots {
			if s.Node == 0 {
				missing++
				continue
			}
			if j < int(c.K) {
				data[s.Node]++
				if !nodes[s.Node].voter {
					dataOffVoters++
				}
			} else {
				parity[s.Node]++
			}
		}
	}
	wantChunks := (size + r.Profile.ChunkSize - 1) / r.Profile.ChunkSize
	r.checkAt("I-9", "every chunk of the large file fully placed", len(reps) == wantChunks && missing == 0,
		fmt.Sprintf("%d chunks (want %d), %d missing slots", len(reps), wantChunks, missing), p, "", "")
	if liveVoters >= k && k > 0 {
		r.checkAt("I-77", "data slots of the large file on voters", dataOffVoters == 0, fmt.Sprintf("%d data slots off the voters (%d live voters, k=%d)", dataOffVoters, liveVoters, k), p, "", "")
	}
	// data slots among voters: proportional to capacity within 10 % or three standard
	// deviations of a binomial draw, whichever is wider (small files are noisy)
	totalData := 0
	for _, n := range data {
		totalData += n
	}
	worst := ""
	balanced := true
	attrs := map[string]string{}
	totalSlots := 0
	for _, n := range data {
		totalSlots += n
	}
	for _, n := range parity {
		totalSlots += n
	}
	// expected shares by capacity, no target expecting more than one slot per chunk (I-9):
	// a big target cannot take a second slot of a stripe, so the rest goes to the others
	allShare, voterShare := map[int]float64{}, map[int]float64{}
	{
		var all, vot []TargetSample
		var allIdx, votIdx []int
		for idx, info := range nodes {
			all = append(all, TargetSample{Capacity: info.cap, Slots: data[idx] + parity[idx], Active: true, Live: true})
			allIdx = append(allIdx, idx)
			if info.voter {
				vot = append(vot, TargetSample{Capacity: info.cap, Slots: data[idx], Active: true, Live: true})
				votIdx = append(votIdx, idx)
			}
		}
		for i, e := range chart.ExpectedShares(all, len(reps)) {
			allShare[allIdx[i]] = e
		}
		for i, e := range chart.ExpectedShares(vot, len(reps)) {
			voterShare[votIdx[i]] = e
		}
	}
	tooFew := false
	judged := 0
	for idx, info := range nodes {
		if capAll > 0 {
			attrs[info.name] = fmt.Sprintf("%d/%d/%.0f", data[idx], parity[idx], allShare[idx])
		}
		if !info.voter || capVoters == 0 {
			continue
		}
		want := voterShare[idx]
		if want < minJudged {
			tooFew = true
			continue
		}
		judged++
		margin := max(0.1*want, spreadSigmas*math.Sqrt(want))
		if got := float64(data[idx]); math.Abs(got-want) > margin {
			balanced = false
			worst = fmt.Sprintf("%s holds %d data slots, expected %.0f ± %.0f", info.name, data[idx], want, margin)
		}
	}
	detail := fmt.Sprintf("%d data slots over %d voters; %s", totalData, len(data), worst)
	if tooFew {
		detail += fmt.Sprintf(" (targets expecting fewer than %d data slots are too few to judge)", minJudged)
	}
	if judged == 0 {
		r.checkSkipped("I-33", "data slots of the large file spread among voters by capacity", fmt.Sprintf("no voter target expects %d data slots or more; the file is too small to judge the spread", minJudged))
	} else {
		r.checkAt("I-33", "data slots of the large file spread among voters by capacity", balanced, detail, p, "", "")
	}
	r.emit(Record{Kind: "event", Name: placementEvent, Text: fmt.Sprintf("%d chunks, %s", len(reps), humanBytes(float64(size))), Attrs: attrs})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return "; " + err.Error()
}
