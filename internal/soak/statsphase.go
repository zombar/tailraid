package soak

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/stats"
)

// --- 20 stats ------------------------------------------------------------------------------
//
// The history of docs/17 on a running cluster. After a few sample intervals every node
// seals its partial hour on demand: its raw segment is under its own directory, decodes,
// and is below the small-chunk threshold; the leader's cluster segment is there too (a).
// A forced rollup writes today's minute and coarse segments, and the minute rows equal
// the pure rollup of the raw rows the driver read back (b). A segment planted with a
// 2010 name is gone after the leader's seal (c). The page's charts render from the rows
// read and the live edge every node answers over /v1/stats (d). A member killed
// mid-phase answers no live edge and the leader's rows mark it dead; nothing writes a
// zero for it (e). The stored bytes grow by no more than docs/17 §6's estimate for one
// sealed hour and one rollup per node, printed measured against estimated (f).

// statsSealBudget is docs/17 §6's bound for one node's sealed hour and rollups: a raw
// segment under 64 KiB replicated three times, a minute and a coarse segment striped.
const statsSealBudget = 3*64<<10 + 2*256<<10

func phaseStats(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Stats {
		r.skip("the environment runs no stats sampler")
		return nil
	}
	gws := r.gateways()
	if len(gws) == 0 {
		r.skip("no gateway reachable")
		return nil
	}
	gw := gws[0]
	v := r.volume(gw)
	var devices []NodeRef
	for _, n := range r.nodes {
		if n.Host == "" {
			devices = append(devices, n)
		}
	}
	before, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	leader := r.leaderRef(ctx)
	// let the samplers run a few intervals
	if err := r.Env.Wait(ctx, 6*time.Second); err != nil {
		return err
	}
	// (a) every live node seals its partial hour; its segment decodes and is small
	sealedAll := true
	for _, n := range devices {
		if !r.alive(ctx, n) {
			continue
		}
		rows, err := r.Env.StatsSeal(ctx, n, false)
		if err != nil || rows == 0 {
			sealedAll = false
			r.checkAt("I-192", "stats seal on "+n.Name, false, fmt.Sprintf("sealed %d rows, err %v", rows, err), "", "", n.Name)
			continue
		}
		r.checkAt("I-192", "stats seal on "+n.Name, true, fmt.Sprintf("sealed %d rows", rows), "", "", n.Name)
	}
	if !sealedAll {
		return nil
	}
	rawRows := map[string][]stats.Row{} // per writer
	for _, n := range devices {
		if !r.alive(ctx, n) {
			continue
		}
		rows, size, ok := r.statsSegments(v, stats.Raw, n.Name)
		r.checkAt("I-192", "raw segment of "+n.Name+" under its own directory", ok && len(rows) > 0 && size < meta.SmallChunkMax,
			fmt.Sprintf("%d rows in %d bytes (threshold %d)", len(rows), size, meta.SmallChunkMax), "", "", n.Name)
		rawRows[n.Name] = rows
	}
	crows, _, ok := r.statsSegments(v, stats.Raw, stats.ClusterWriter)
	r.check("I-191", "the leader's cluster segment", ok && len(crows) > 0 && crows[0].C != nil && crows[0].Term > 0, fmt.Sprintf("%d cluster rows, term %d", len(crows), firstTerm(crows)))
	rawRows[stats.ClusterWriter] = crows
	// (b) a forced rollup: today's minute rows equal the pure rollup of the raw rows read
	if _, err := r.Env.StatsSeal(ctx, gw, true); err != nil {
		r.check("I-193", "stats seal --rollup", false, err.Error())
		return nil
	}
	// the rollup seal sealed the rows sampled since the first seal into a second raw
	// segment before rolling the day up: the raw rows are read again, after it, since a
	// segment changes only on a seal
	rawNow, _, _ := r.statsSegments(v, stats.Raw, gw.Name)
	minute, _, ok := r.statsSegments(v, stats.Minute, gw.Name)
	want := stats.Rollup(rawNow, stats.Minute)
	var wantBuf, gotBuf bytes.Buffer
	_ = stats.Encode(&wantBuf, want)
	_ = stats.Encode(&gotBuf, minute)
	r.check("I-193", "today's minute rollup equals the rollup of the raw rows", ok && bytes.Equal(wantBuf.Bytes(), gotBuf.Bytes()),
		fmt.Sprintf("%d minute rows from %d raw rows", len(minute), len(rawNow)))
	if coarse, _, ok := r.statsSegments(v, stats.Coarse, gw.Name); !ok || len(coarse) == 0 {
		r.check("I-193", "today's coarse rollup", false, "no coarse segment")
	} else {
		r.check("I-193", "today's coarse rollup", true, fmt.Sprintf("%d coarse rows", len(coarse)))
	}
	// (c) retention: a segment planted with a 2010 name is gone after the leader's seal
	planted := path.Join(stats.Root, stats.Raw.Dir(), "gone", stats.SegmentName(stats.Raw, time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC), "gone"))
	var seg bytes.Buffer
	_ = stats.Encode(&seg, []stats.Row{{T: 1262304000, N: "gone"}})
	if err := v.Mkdir(path.Dir(planted)); err == nil {
		err = v.WriteAt(planted, 0, seg.Bytes())
		if err == nil {
			_ = r.Env.Wait(ctx, time.Second) // the write commits
		}
	}
	if _, err := r.Env.StatsSeal(ctx, leader, false); err != nil {
		r.check("I-194", "the leader's seal prunes", false, err.Error())
	} else {
		// a kernel NFS client holds the name it wrote in its attribute cache for up to
		// acdirmax (docs/07): the mount is given that window to show the removal
		gone := r.waitGone(ctx, v, planted)
		r.check("I-194", "a segment past its retention is removed, whoever wrote it", gone, planted)
	}
	// (d) the page's charts from the rows read and the live edge of every node
	var all []stats.Row
	for _, rows := range rawRows {
		all = append(all, rows...)
	}
	liveNodes := 0
	for _, n := range devices {
		if !r.alive(ctx, n) {
			continue
		}
		rows, err := r.Env.StatsLive(ctx, n, 0)
		if err != nil {
			r.checkAt("I-196", "live edge of "+n.Name, false, err.Error(), "", "", n.Name)
			continue
		}
		liveNodes++
		all = append(all, rows...)
	}
	now := time.Unix(maxT(all), 0).UTC().Add(time.Second)
	charts := stats.Charts(all, now.Add(-6*time.Hour), now, gw.Name)
	ids := map[string]bool{}
	for _, c := range charts {
		ids[c.ID] = true
	}
	r.check("I-196", "the page's charts render for a 6 h range", ids["stored"] && ids["liveness"] && ids["cpu"] && liveNodes > 0,
		fmt.Sprintf("%d charts from %d rows, %d live edges", len(charts), len(all), liveNodes))
	// (e) a killed member leaves a gap: no live edge, marked dead by the leader
	if r.Env.Caps().Kill && len(devices) >= 3 {
		var victim NodeRef
		found := false
		for _, n := range devices {
			if n.Idx != leader.Idx && n.Idx != gw.Idx && r.alive(ctx, n) {
				victim, found = n, true
				break
			}
		}
		if found {
			u := r.undoer(ctx)
			defer u.run()
			if err := r.Env.Kill(ctx, victim); err != nil {
				return err
			}
			u.add("start "+victim.Name, func(c context.Context) error { return r.Env.Start(c, victim) })
			r.event("kill", victim.Name)
			killed := r.statsNow(ctx, devices)
			// the prober marks the member dead after LiveThreshold and the leader's next
			// rows carry it (the harness's Kill advances its clock past the threshold
			// already); leadership may move meanwhile, so every live member is asked and the
			// newest cluster row written after the kill is the one judged, within the window
			_ = r.Env.Wait(ctx, cluster.LiveThreshold+2*r.Profile.SampleEvery)
			_, err := r.Env.StatsLive(ctx, victim, 0)
			if err == nil && r.Env.Caps().Self != "" && r.alive(ctx, victim) {
				// a live cluster's kill holds the victim off every member but the device
				// running verify, whose path stays for the lift: the victim still answers
				// it, so the rows the leader writes (below) are the judge here
				r.checkSkipped("I-196", "a dead member has no live edge", "the device running verify keeps its path to a killed node (docs/09 §6)")
			} else {
				r.checkAt("I-196", "a dead member has no live edge", err != nil, fmt.Sprintf("err %v", err), "", "", victim.Name)
			}
			deadline := time.Now().Add(attrCacheWindow)
			var last *stats.Row
			var by string
			for {
				last, by = r.newestCluster(ctx, devices, victim, killed)
				if (last != nil && last.C.Live[victim.Name] == 0) || time.Now().After(deadline) || r.Env.Wait(ctx, 2*time.Second) != nil {
					break
				}
			}
			r.checkAt("I-191", "the leader's row marks the dead member", last != nil && last.C.Live[victim.Name] == 0,
				fmt.Sprintf("newest cluster row after the kill from %s: live %v", by, liveOf(last)), "", "", victim.Name)
		}
	} else {
		r.checkSkipped("I-196", "a dead member has no live edge", "kills are not allowed on this environment")
	}
	// (f) the stored bytes against the budget
	after, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	delta := int64(after.Volume.StoredBytes) - int64(before.Volume.StoredBytes) //nolint:gosec // bounded sizes
	bound := int64(len(devices)+1) * statsSealBudget
	r.event("stats budget", fmt.Sprintf("stored +%d bytes for %d nodes' seal and rollups; docs/17 §6 allows %d", delta, len(devices), bound))
	r.check("I-192", "the seal and rollups stay within docs/17 §6's budget", delta <= bound, fmt.Sprintf("+%d bytes of %d", delta, bound))
	return nil
}

// statsSegments reads a writer's segments at a tier through a gateway's volume: the rows
// in name order, the largest segment's size, and whether the directory had any.
func (r *Run) statsSegments(v Volume, tier stats.Tier, writer string) ([]stats.Row, int64, bool) {
	dir := path.Join(stats.Root, tier.Dir(), writer)
	names, err := v.List(dir)
	if err != nil {
		return nil, 0, false
	}
	var rows []stats.Row
	var largest int64
	found := false
	for _, n := range names {
		if _, ok := stats.ParseName(tier, n); !ok {
			continue
		}
		data, err := v.ReadAll(path.Join(dir, n))
		if err != nil {
			continue
		}
		got, err := stats.Decode(bytes.NewReader(data))
		if err != nil {
			continue
		}
		found = true
		largest = max(largest, int64(len(data)))
		rows = append(rows, got...)
	}
	return rows, largest, found
}

func (r *Run) alive(ctx context.Context, n NodeRef) bool {
	_, err := r.Env.Status(ctx, n)
	return err == nil
}

func firstTerm(rows []stats.Row) uint64 {
	if len(rows) == 0 {
		return 0
	}
	return rows[0].Term
}

func maxT(rows []stats.Row) int64 {
	var t int64
	for _, r := range rows {
		t = max(t, r.T)
	}
	return t
}

func lastCluster(rows []stats.Row) *stats.Row {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].C != nil {
			return &rows[i]
		}
	}
	return nil
}

func liveOf(r *stats.Row) map[string]int {
	if r == nil {
		return nil
	}
	return r.C.Live
}

// waitGone returns true once the mount no longer answers a stat of p, within the
// attribute-cache window (docs/07).
func (r *Run) waitGone(ctx context.Context, v Volume, p string) bool {
	deadline := time.Now().Add(attrCacheWindow)
	for {
		if _, err := v.Size(p); err != nil {
			return true
		}
		if time.Now().After(deadline) || r.Env.Wait(ctx, 2*time.Second) != nil {
			return false
		}
	}
}

// statsNow is the newest instant any live member has sampled: the clock the rows carry.
func (r *Run) statsNow(ctx context.Context, devices []NodeRef) int64 {
	var t int64
	for _, n := range devices {
		if rows, err := r.Env.StatsLive(ctx, n, 0); err == nil {
			t = max(t, maxT(rows))
		}
	}
	return t
}

// newestCluster asks every live member but victim for its unsealed rows after since
// and returns the newest cluster row among them, with the member that held it.
func (r *Run) newestCluster(ctx context.Context, devices []NodeRef, victim NodeRef, since int64) (*stats.Row, string) {
	var best *stats.Row
	by := "nobody"
	for _, n := range devices {
		if n.Idx == victim.Idx {
			continue
		}
		rows, err := r.Env.StatsLive(ctx, n, since)
		if err != nil {
			continue
		}
		if c := lastCluster(rows); c != nil && (best == nil || c.T > best.T || (c.T == best.T && c.Term > best.Term)) {
			best, by = c, n.Name
		}
	}
	return best, by
}
