package soak

import (
	"context"
	"fmt"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// --- 11 automation ---------------------------------------------------------------------------
//
// The three things the cluster decides for itself (docs/01 §Node budget, docs/03): every
// device's budget follows its data disk, data slots sit on voters, and in auto mode the
// policy equals the rule of the member table. Each check lands in the report with the
// invariant it defends.

const (
	fillBytes   = 2 << 30           // what the phase consumes on one node's disk
	fillWait    = 150 * time.Second // two budget ticks and a probe
	fillStep    = 5 * time.Second   // poll interval while waiting for the store cap to follow
	minFillFree = 4 * fillBytes     // leave the rig's disk alone below this
	sampleFiles = 24                // corpus files whose placement is inspected
)

func phaseAutomation(ctx context.Context, r *Run) error {
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	if r.Env.Caps().Corrupt && r.Env.Caps().Mutate { // the dev endpoints are reachable: budgets can be measured
		if err := r.budgetChecks(ctx, rep); err != nil {
			return err
		}
	} else {
		r.checkSkipped("I-74", "the store's cap follows the disk", "budget checks need the dev endpoints (run with dev.enabled and --destructive)")
	}
	if err := r.voterFirstChecks(ctx); err != nil {
		return err
	}
	if !r.Env.Caps().Mutate {
		r.checkSkipped("I-75", "policy equals the rule of the member table", "a policy change needs --destructive on a live cluster")
		return nil
	}
	return r.autoPolicyChecks(ctx, rep)
}

func reportNodeByName(rep admin.Report, name string) (admin.NodeReport, bool) {
	for i := range rep.Nodes {
		if rep.Nodes[i].Name == name {
			return rep.Nodes[i], true
		}
	}
	return admin.NodeReport{}, false
}

func reportNode(rep admin.Report, idx meta.NodeIdx) (admin.NodeReport, bool) {
	for i := range rep.Nodes {
		if rep.Nodes[i].Idx == idx {
			return rep.Nodes[i], true
		}
	}
	return admin.NodeReport{}, false
}

// budgetChecks: every derived disk budget is within the hysteresis of what its filesystem
// says (I-74); after a scratch file consumes part of one disk, that disk's free space as
// the store reports it drops within two ticks (I-74). A device row has no budget (docs/14
// §9), so the checks run per disk, matched to status by name; a pinned disk is skipped.
func (r *Run) budgetChecks(ctx context.Context, rep admin.Report) error {
	gw := r.gateways()[0]
	var victim NodeRef
	var victimDisk DiskInfo
	for _, n := range r.nodes {
		ds, err := r.Env.Disks(ctx, n)
		if err != nil {
			r.checkAt("I-74", n.Name+" reports its disks", false, err.Error(), "", "", n.Name)
			continue
		}
		for _, d := range ds {
			if d.Pinned {
				continue
			}
			nr, ok := reportNodeByName(rep, d.Name)
			if !ok {
				continue
			}
			want := chunk.Budget(d.Used, d.Free, d.Total)
			r.checkAt("I-74", d.Name+" budget follows its disk", !chunk.BudgetChanged(want, nr.Capacity),
				fmt.Sprintf("status %s, disk says %s (held %s, free %s of %s)", humanBytes(float64(nr.Capacity)), humanBytes(float64(want)), humanBytes(float64(d.Used)), humanBytes(float64(d.Free)), humanBytes(float64(d.Total))),
				"", "", n.Name)
			if n.Idx != gw.Idx && nr.Live && d.Free >= minFillFree && victim.Idx == 0 {
				victim, victimDisk = n, d
			}
		}
	}
	if victim.Idx == 0 {
		r.event("note", "fill skipped: no non-gateway node with a derived disk and enough free space")
		return nil
	}
	before, _ := reportNodeByName(rep, victimDisk.Name)
	if err := r.Env.Fill(ctx, victim, victimDisk.ID, fillBytes); err != nil {
		r.checkAt("I-74", "fill accepted", false, err.Error(), "", "", victim.Name)
		return nil
	}
	r.event("fill", fmt.Sprintf("%s on %s (free was %s)", humanBytes(fillBytes), victimDisk.Name, humanBytes(float64(victimDisk.Free))))
	u := r.undoer(ctx)
	defer u.run()
	u.add("unfill "+victimDisk.Name, func(c context.Context) error { return r.Env.Fill(c, victim, victimDisk.ID, 0) })
	deadline := r.now().Add(fillWait)
	var after admin.NodeReport
	answered := false
	for {
		if rep, err := r.Env.Status(ctx, victim); err == nil {
			if nr, ok := reportNodeByName(rep, victimDisk.Name); ok {
				after, answered = nr, true
				if before.Free-after.Free >= fillBytes/2 {
					break
				}
			}
		}
		if r.now().After(deadline) {
			break
		}
		if err := r.Env.Wait(ctx, fillStep); err != nil {
			return err
		}
	}
	if !answered { // a victim that never answered status is not a pass, and not a budget finding either
		r.checkSkipped("I-74", "the store's cap follows the disk within two ticks", victim.Name+" did not answer status while filled")
		return nil
	}
	r.checkAt("I-74", "the store's cap follows the disk within two ticks", before.Free-after.Free >= fillBytes/2,
		fmt.Sprintf("%s: free %s → %s after a %s fill", victimDisk.Name, humanBytes(float64(before.Free)), humanBytes(float64(after.Free)), humanBytes(fillBytes)), "", "", victim.Name)
	return nil
}

// voterFirstChecks: for the corpus this run wrote, every chunk's data slots sit on voters
// while at least k voters are live (I-77). Home placement holds a data slot that a refill
// or migration put on a non-voter (a voter disk that could not take a shard just then)
// until the home gate moves it, and a pass moves nothing home while any chunk is bad
// (docs/14 §7, I-295), so the earlier phases' faults can leave the corpus away; the check
// runs repair until the gate shuts first (soak 2026-10-17: phase 7's migrations ran five
// seconds after n1's restart and the moves were still queued when the phase began).
func (r *Run) voterFirstChecks(ctx context.Context) error {
	rep, err := r.repairUntilClean(ctx)
	if err != nil {
		return err
	}
	vt := voterTargets(rep) // a disk of a voter is a voter target (docs/14 §5)
	voters := map[int]bool{}
	liveVoters := 0
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if vt[int(n.Idx)] && n.State == "active" {
			voters[int(n.Idx)] = true
			if n.Live {
				liveVoters++
			}
		}
	}
	paths := sortedPaths(r.corpusModel())
	if len(paths) > sampleFiles {
		paths = paths[:sampleFiles]
	}
	chunks, bad, first := 0, 0, ""
	for _, p := range paths {
		reps, err := r.chunksOf(ctx, p)
		if err != nil {
			continue
		}
		for ci := range reps {
			c := &reps[ci]
			if int(c.K) > liveVoters || !c.Found {
				continue
			}
			chunks++
			for i := 0; i < int(c.K) && i < len(c.Slots); i++ {
				if s := c.Slots[i]; s.Node != 0 && !voters[s.Node] {
					bad++
					if first == "" {
						first = fmt.Sprintf("%s chunk %s data slot %d on non-voter %s: %s", p, c.Hash[:12], i, s.Name, slotTable(*c))
					}
					break
				}
			}
		}
	}
	h := r.lastRepair // the last pass repairUntilClean ran: what was still away from home
	r.check("I-77", "data slots of the corpus sit on voters", bad == 0 && chunks > 0,
		fmt.Sprintf("%d chunks inspected over %d files, %d with a data slot off the voters (home: %d away, %s movable, gate open %v); %s", chunks, len(paths), bad, h.AwaySlots, humanBytes(float64(h.MovableBytes)), h.HomeOpen, first))
	return nil
}

// autoPolicyChecks: auto mode moves the policy to AutoPolicy of the member table and the
// migration completes with the corpus exact (I-75, I-32, I-12); a manual cluster is pinned
// back afterwards so the rest of the run (and a user's cluster) is as it was.
func (r *Run) autoPolicyChecks(ctx context.Context, rep admin.Report) error {
	gw := r.gateways()[0]
	v := r.volume(gw)
	m := r.corpusModel()
	k0, m0, wasAuto := rep.Volume.K, rep.Volume.M, rep.Volume.Auto
	if err := r.Env.SetPolicyAuto(ctx); err != nil {
		r.check("I-75", "set-redundancy --auto accepted", false, err.Error())
		return nil
	}
	u := r.undoer(ctx)
	defer u.run()
	if !wasAuto {
		u.add("pin the policy back", func(c context.Context) error { return r.Env.SetPolicy(c, k0, m0) })
	}
	r.event("set-redundancy", "auto")
	rep, err := r.repairUntilClean(ctx)
	if err != nil {
		return err
	}
	members, err := membersOf(rep)
	if err != nil {
		return err
	}
	wantK, wantM := meta.AutoPolicy(members)
	r.check("I-75", "policy equals the rule of the member table", rep.Volume.Auto && rep.Volume.K == wantK && rep.Volume.M == wantM,
		fmt.Sprintf("policy %d+%d (auto %v), members call for %d+%d; was %d+%d", rep.Volume.K, rep.Volume.M, rep.Volume.Auto, wantK, wantM, k0, m0))
	r.check("I-15 I-32", "migration to the automatic policy complete", rep.Volume.StalePolicy == 0 && rep.Volume.Degraded == 0,
		fmt.Sprintf("%d stale, %d degraded", rep.Volume.StalePolicy, rep.Volume.Degraded))
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-12 I-32", "corpus exact under the automatic policy", bad == 0, first, first, "", gw.Name)
	if wasAuto {
		return nil
	}
	if err := r.Env.SetPolicy(ctx, k0, m0); err != nil {
		r.check("L-100", fmt.Sprintf("set-redundancy %d+%d (pin back) accepted", k0, m0), false, err.Error())
		return nil
	}
	u.done("pin the policy back")
	r.event("set-redundancy", fmt.Sprintf("%d+%d (pinned back)", k0, m0))
	if rep, err = r.repairUntilClean(ctx); err != nil {
		return err
	}
	r.check("I-15 I-32", fmt.Sprintf("migration back to %d+%d complete", k0, m0), !rep.Volume.Auto && rep.Volume.StalePolicy == 0 && rep.Volume.Degraded == 0,
		fmt.Sprintf("%d stale, %d degraded, auto %v", rep.Volume.StalePolicy, rep.Volume.Degraded, rep.Volume.Auto))
	return nil
}

// membersOf rebuilds the member table AutoPolicy needs from a status report. A kind or
// state the report spells in a way this binary does not know is an error, never a guess.
func membersOf(rep admin.Report) ([]meta.Node, error) {
	out := make([]meta.Node, 0, len(rep.Nodes))
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		kind, err := meta.ParseNodeKind(n.Kind)
		if err != nil {
			return nil, err
		}
		state, err := meta.ParseNodeState(n.State)
		if err != nil {
			return nil, err
		}
		out = append(out, meta.Node{Idx: n.Idx, Voter: n.Voter, Capacity: n.Capacity, Host: n.Host, Kind: kind, State: state})
	}
	return out, nil
}
