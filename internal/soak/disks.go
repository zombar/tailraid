package soak

import (
	"context"
	"fmt"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
)

// --- 13 disks ------------------------------------------------------------------------------

// phaseDisks exercises several disks per device (docs/14): every chunk within the host cap
// and on distinct targets; chunks piled onto one host while the others drain are not
// reported skewed, and are moved within the cap once the hosts return; a disk that closes
// while its host runs is dead at once, reads stay exact, and its slots are re-placed (or,
// with no spare target, healed once it returns).
func phaseDisks(ctx context.Context, r *Run) error {
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	if !hasDisks(rep) {
		r.skip("no host has an added disk")
		return nil
	}
	r.capChecks(ctx, rep, "as written")
	if err := r.skewChecks(ctx, rep); err != nil {
		return err
	}
	if !r.Env.Caps().Disks || !r.Env.Caps().Kill {
		r.event("note", "disk failure skipped: the environment cannot close a disk")
	} else if err := r.diskFailureChecks(ctx); err != nil {
		return err
	}
	r.checkFSMAgreement(ctx, "I-4", "FSM hashes agree after the disk phase")
	return nil
}

// liveHosts is how many hosts have an active, live target: the D of the host cap.
func liveHosts(rep admin.Report) int {
	seen := map[string]bool{}
	for _, t := range targetsOf(rep) {
		if t.Active && t.Live && t.Capacity > 0 {
			seen[t.Host] = true
		}
	}
	return len(seen)
}

// capChecks: across the corpus, no chunk has two slots on one target (I-9) or more than
// ceil((k+m)/D) on one host (I-87, I-89).
func (r *Run) capChecks(ctx context.Context, rep admin.Report, when string) {
	d := max(liveHosts(rep), 1)
	paths := sortedPaths(r.corpusModel())
	if len(paths) > 40 {
		paths = paths[:40]
	}
	chunks, over, dup, first := 0, 0, 0, ""
	for _, p := range paths {
		reps, err := r.chunksOf(ctx, p)
		if err != nil {
			continue
		}
		for ci := range reps {
			c := &reps[ci]
			if !c.Found {
				continue
			}
			chunks++
			limit := (len(c.Slots) + d - 1) / d
			perHost, seen := map[int]int{}, map[int]bool{}
			for _, s := range c.Slots {
				if s.Node == 0 {
					continue
				}
				if seen[s.Node] {
					dup++
				}
				seen[s.Node] = true
				perHost[r.hostIdx(s.Node)]++
			}
			for host, n := range perHost {
				if n > limit {
					over++
					if first == "" {
						first = fmt.Sprintf("%s chunk %s: %d slots on host %d, cap %d: %s", p, c.Hash[:12], n, host, limit, slotTable(*c))
					}
					break
				}
			}
		}
	}
	r.check("I-9", "no chunk holds two slots on one target ("+when+")", dup == 0 && chunks > 0, fmt.Sprintf("%d chunks, %d with a target twice", chunks, dup))
	r.check("I-87 I-89", "every chunk within the host cap ("+when+")", over == 0 && chunks > 0, fmt.Sprintf("%d chunks over %d hosts, %d above the cap; %s", chunks, d, over, first))
}

// skewChecks drains every host but the gateway's, so repair piles the corpus onto the
// gateway host's targets with no skew reported (no placement within the cap exists), then
// reactivates them: the skew is reported and repair moves it within the cap (L-193, L-194).
func (r *Run) skewChecks(ctx context.Context, rep admin.Report) error {
	gw := r.gateways()[0]
	if len(r.nodes) < 2 {
		r.event("note", "skew skipped: one host")
		return nil
	}
	gwTargets := 0
	for _, t := range targetsOf(rep) {
		if t.Host == gw.Name && t.Active && t.Capacity > 0 {
			gwTargets++
		}
	}
	if n := int(rep.Volume.K) + int(rep.Volume.M); gwTargets < n {
		r.event("note", fmt.Sprintf("skew skipped: %s has %d targets, a stripe needs %d", gw.Name, gwTargets, n))
		return nil
	}
	var others []NodeRef
	for _, n := range r.nodes {
		if n.Idx != gw.Idx {
			others = append(others, n)
		}
	}
	for _, n := range others {
		if err := r.Env.SetState(ctx, n, meta.Draining); err != nil {
			r.check("L-93", "drain accepted", false, err.Error())
			return nil
		}
		r.event("drain", n.Name)
	}
	rep, err := r.repairUntilClean(ctx)
	if err != nil {
		return err
	}
	piled := 0
	for _, n := range others {
		piled += hostSlots(rep, int(n.Idx))
	}
	r.check("I-42", "every slot piled onto the gateway host while the others drain", piled == 0, fmt.Sprintf("%d slots left on the draining hosts", piled))
	r.check("L-194", "no skew reported while no placement within the cap exists", rep.Volume.DomainSkew == 0, fmt.Sprintf("%d skewed chunks", rep.Volume.DomainSkew))
	for _, n := range others {
		if err := r.Env.SetState(ctx, n, meta.Active); err != nil {
			return err
		}
		r.event("activate", n.Name)
	}
	if rep, _, err = r.anyStatus(ctx); err != nil {
		return err
	}
	r.check("L-193", "skew reported once the hosts return", rep.Volume.DomainSkew > 0, fmt.Sprintf("%d skewed chunks", rep.Volume.DomainSkew))
	if rep, err = r.repairUntilClean(ctx); err != nil {
		return err
	}
	r.check("I-89 L-193", "repair moves every skewed chunk within the cap", rep.Volume.DomainSkew == 0 && rep.Volume.Degraded == 0,
		fmt.Sprintf("%d skewed, %d degraded", rep.Volume.DomainSkew, rep.Volume.Degraded))
	r.capChecks(ctx, rep, "after the skew moves")
	v, _ := r.volume(gw)
	bad, first := r.verifyModel(v, r.corpusModel())
	r.checkAt("I-89 I-32", "corpus exact after the skew moves", bad == 0, first, first, "", gw.Name)
	return nil
}

// diskFailureChecks closes a disk (preferably not on the gateway's host), checks it is
// dead while its host is live and reads stay exact, lets repair re-place its slots when a
// spare target exists, then restores it (I-90, L-192).
func (r *Run) diskFailureChecks(ctx context.Context) error {
	gw := r.gateways()[0]
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	disks := diskRefs(rep)
	if len(disks) == 0 {
		r.event("note", "disk failure skipped: no active disk")
		return nil
	}
	victim := disks[0]
	for _, d := range disks {
		if d.Host != gw.Name {
			victim = d
			break
		}
	}
	active := 0
	for _, t := range targetsOf(rep) {
		if t.Active && t.Capacity > 0 {
			active++
		}
	}
	spare := active > int(rep.Volume.K)+int(rep.Volume.M)
	if err := r.Env.FailDisk(ctx, victim); err != nil {
		r.check("I-90", "disk closed", false, err.Error())
		return nil
	}
	r.event("fail disk", victim.Name)
	healthy, detail := r.waitHealthy(ctx, r.Profile.RestartSettle+30*time.Second, victim.Name)
	r.check("L-132", "cluster healthy with a disk closed", healthy, detail)
	if err := r.Env.Wait(ctx, cluster.ProbeInterval+time.Second); err != nil {
		return err
	}
	if rep, _, err = r.anyStatus(ctx); err != nil {
		return err
	}
	diskLive, hostLive := true, false
	for i := range rep.Nodes {
		switch rep.Nodes[i].Name {
		case victim.Name:
			diskLive = rep.Nodes[i].Live
		case victim.Host:
			hostLive = rep.Nodes[i].Live
		}
	}
	r.checkAt("I-90 L-192", "the closed disk is dead while its host is live", !diskLive && hostLive, fmt.Sprintf("disk live %v, host live %v", diskLive, hostLive), "", "", victim.Name)
	v, _ := r.volume(gw)
	m := r.corpusModel()
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-90 I-12", "reads exact with a disk closed", bad == 0, first, first, "", gw.Name)
	if spare {
		if err := r.Env.Wait(ctx, r.Profile.FillAfter+time.Second); err != nil {
			return err
		}
		if rep, err = r.repairUntilClean(ctx); err != nil {
			return err
		}
		left := hostSlotsOf(rep, victim.Name)
		r.checkAt("I-90 L-192", "repair re-places the closed disk's slots after fill_after", left == 0 && rep.Volume.Degraded == 0,
			fmt.Sprintf("%d slots left on it, %d degraded", left, rep.Volume.Degraded), "", "", victim.Name)
		r.capChecks(ctx, rep, "after the disk's slots moved")
	} else {
		r.event("note", "no spare target: the closed disk's slots wait for it to return")
	}
	if err := r.Env.RestoreDisk(ctx, victim); err != nil {
		return err
	}
	r.event("restore disk", victim.Name)
	healthy, detail = r.waitHealthy(ctx, r.Profile.RestartSettle+30*time.Second)
	r.check("L-132", "cluster healthy after the disk returned", healthy, detail)
	if err := r.Env.Wait(ctx, cluster.ProbeInterval+time.Second); err != nil {
		return err
	}
	if rep, err = r.repairUntilClean(ctx); err != nil {
		return err
	}
	diskLive = false
	for i := range rep.Nodes {
		if rep.Nodes[i].Name == victim.Name {
			diskLive = rep.Nodes[i].Live
		}
	}
	r.checkAt("I-90", "the restored disk is live again", diskLive, "", "", "", victim.Name)
	r.check("I-15", "no degraded chunk after the disk returned", rep.Volume.Degraded == 0 && rep.Volume.Unreadable == 0, fmt.Sprintf("%d degraded", rep.Volume.Degraded))
	bad, first = r.verifyModel(v, m)
	r.checkAt("I-12", "corpus exact after the disk returned", bad == 0, first, first, "", gw.Name)
	return nil
}

// hostSlotsOf is the slots the named target holds.
func hostSlotsOf(rep admin.Report, name string) int {
	for i := range rep.Nodes {
		if rep.Nodes[i].Name == name {
			return rep.Nodes[i].Slots
		}
	}
	return 0
}
