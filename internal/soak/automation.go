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
	if r.Env.Caps().Corrupt { // the dev endpoints are reachable: budgets can be measured
		if err := r.budgetChecks(ctx, rep); err != nil {
			return err
		}
	} else {
		r.event("note", "budget checks need the dev endpoints (run with dev.enabled and --destructive)")
	}
	r.voterFirstChecks(ctx, rep)
	return r.autoPolicyChecks(ctx, rep)
}

func reportNode(rep admin.Report, idx meta.NodeIdx) (admin.NodeReport, bool) {
	for i := range rep.Nodes {
		if rep.Nodes[i].Idx == idx {
			return rep.Nodes[i], true
		}
	}
	return admin.NodeReport{}, false
}

// budgetChecks: every budget is within the hysteresis of what its disk says (I-74); after
// a scratch file consumes part of one node's disk, that node's free space as the store
// reports it drops within two ticks (I-74).
func (r *Run) budgetChecks(ctx context.Context, rep admin.Report) error {
	gw := r.gateways()[0]
	var victim NodeRef
	var victimDisk DiskInfo
	for _, n := range r.nodes {
		d, err := r.Env.Disk(ctx, n)
		if err != nil {
			r.checkAt("I-74", n.Name+" reports its disk", false, err.Error(), "", "", n.Name)
			continue
		}
		nr, ok := reportNode(rep, n.Idx)
		if !ok {
			continue
		}
		want := chunk.Budget(d.Used, d.Free, d.Total)
		r.checkAt("I-74", n.Name+" budget follows its disk", !chunk.BudgetChanged(want, nr.Capacity),
			fmt.Sprintf("status %s, disk says %s (held %s, free %s of %s)", humanBytes(float64(nr.Capacity)), humanBytes(float64(want)), humanBytes(float64(d.Used)), humanBytes(float64(d.Free)), humanBytes(float64(d.Total))),
			"", "", n.Name)
		if n.Idx != gw.Idx && nr.Live && d.Free >= minFillFree {
			victim, victimDisk = n, d
		}
	}
	if victim.Idx == 0 {
		r.event("note", "fill skipped: no non-gateway node with enough free disk")
		return nil
	}
	before, _ := reportNode(rep, victim.Idx)
	if err := r.Env.Fill(ctx, victim, fillBytes); err != nil {
		r.checkAt("I-74", "fill accepted", false, err.Error(), "", "", victim.Name)
		return nil
	}
	r.event("fill", fmt.Sprintf("%s on %s (free was %s)", humanBytes(fillBytes), victim.Name, humanBytes(float64(victimDisk.Free))))
	defer func() { _ = r.Env.Fill(ctx, victim, 0) }()
	deadline := r.now().Add(fillWait)
	var after admin.NodeReport
	for {
		if rep, err := r.Env.Status(ctx, victim); err == nil {
			if nr, ok := reportNode(rep, victim.Idx); ok {
				after = nr
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
	r.checkAt("I-74", "the store's cap follows the disk within two ticks", before.Free-after.Free >= fillBytes/2,
		fmt.Sprintf("free %s → %s after a %s fill", humanBytes(float64(before.Free)), humanBytes(float64(after.Free)), humanBytes(fillBytes)), "", "", victim.Name)
	return nil
}

// voterFirstChecks: for the corpus this run wrote, every chunk's data slots sit on voters
// while at least k voters are live (I-77).
func (r *Run) voterFirstChecks(ctx context.Context, rep admin.Report) {
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
	r.check("I-77", "data slots of the corpus sit on voters", bad == 0 && chunks > 0,
		fmt.Sprintf("%d chunks inspected over %d files, %d with a data slot off the voters; %s", chunks, len(paths), bad, first))
}

// autoPolicyChecks: auto mode moves the policy to AutoPolicy of the member table and the
// migration completes with the corpus exact (I-75, I-32, I-12); a manual cluster is pinned
// back afterwards so the rest of the run (and a user's cluster) is as it was.
func (r *Run) autoPolicyChecks(ctx context.Context, rep admin.Report) error {
	gw := r.gateways()[0]
	v, _ := r.volume(gw)
	m := r.corpusModel()
	k0, m0, wasAuto := rep.Volume.K, rep.Volume.M, rep.Volume.Auto
	if err := r.Env.SetPolicyAuto(ctx); err != nil {
		r.check("I-75", "set-redundancy --auto accepted", false, err.Error())
		return nil
	}
	r.event("set-redundancy", "auto")
	rep, err := r.repairUntilClean(ctx)
	if err != nil {
		return err
	}
	wantK, wantM := meta.AutoPolicy(membersOf(rep))
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
	r.event("set-redundancy", fmt.Sprintf("%d+%d (pinned back)", k0, m0))
	if rep, err = r.repairUntilClean(ctx); err != nil {
		return err
	}
	r.check("I-15 I-32", fmt.Sprintf("migration back to %d+%d complete", k0, m0), !rep.Volume.Auto && rep.Volume.StalePolicy == 0 && rep.Volume.Degraded == 0,
		fmt.Sprintf("%d stale, %d degraded, auto %v", rep.Volume.StalePolicy, rep.Volume.Degraded, rep.Volume.Auto))
	return nil
}

// membersOf rebuilds the member table AutoPolicy needs from a status report.
func membersOf(rep admin.Report) []meta.Node {
	out := make([]meta.Node, 0, len(rep.Nodes))
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		nd := meta.Node{Idx: n.Idx, Voter: n.Voter, Capacity: n.Capacity, Host: n.Host}
		switch n.Kind {
		case "device":
			nd.Kind = meta.KindDevice
		case "disk":
			nd.Kind = meta.KindDisk
		default:
			nd.Kind = meta.KindS3
		}
		switch n.State {
		case "active":
			nd.State = meta.Active
		case "draining":
			nd.State = meta.Draining
		default:
			nd.State = meta.Removed
		}
		out = append(out, nd)
	}
	return out
}
