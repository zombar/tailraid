package soak_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
)

// Phase 11's voter check meets the corpus as the earlier phases left it: a voter's disk
// that could not take shards while repair refilled put data slots on non-voters, and
// those wait for the home gate (I-295) after the disk is back. The check runs repair
// until the gate shuts before it looks, so the moves home are done and it passes; it
// looked at once before (soak 2026-10-17: 24 of 32 chunks with a data slot off the
// voters, every away slot movable and the gate open).
// Invariant: I-77, I-295
func TestSoak_VoterCheckAfterDataSlotsWentAway(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1, testharness.NonVoters(2, 3))
	t.Cleanup(c.Close)
	disks := make([]meta.NodeIdx, len(c.Nodes)) // each host's default disk: 4 hosts × 1 target
	for i := range c.Nodes {
		disks[i] = c.Nodes[i].Target()
	}
	c.AddDisklessNode(true) // three voters, as the compose rig's n5
	env := &harnessEnv{t: t, c: c}
	displace := soak.Phase{Name: "displace", Run: func(ctx context.Context, r *soak.Run) error {
		c.FailDisk(0, disks[0], true) // a voter's disk: its data slots refill on the non-voters
		c.Probe()                     // seen dead now, so fill_after runs from here
		if err := env.Wait(ctx, r.Profile.FillAfter+time.Second); err != nil {
			return err
		}
		repaired := 0
		for range 16 {
			st, err := env.RepairNow(ctx)
			if err != nil {
				return err
			}
			if repaired += st.Repaired; st.Repaired == 0 && repaired > 0 {
				break
			}
		}
		r.Check("I-77", "the voter's disk was refilled elsewhere", repaired > 0, fmt.Sprintf("%d chunks repaired", repaired))
		c.FailDisk(0, disks[0], false) // back, and no pass has run since
		c.Probe()
		return nil
	}}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	phases := append(soak.StandardPhases("1"), displace)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: append(phases, soak.StandardPhases("11")...)}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ph := range sum.Phases {
		for _, chk := range ph.Checks {
			if chk.OK != nil && !*chk.OK {
				t.Errorf("%s: %s: %s", ph.Name, chk.Name, chk.Text)
			}
			if strings.HasPrefix(chk.Name, "data slots of the corpus sit on voters") {
				found = chk.OK != nil
			}
		}
	}
	if !found {
		t.Fatal("phase 11 ran no voter check")
	}
}
