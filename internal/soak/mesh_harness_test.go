package soak_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
)

// The mesh phases report themselves skipped, by name, on an environment with no mesh of
// its own (the in-process harness, docs/09 §4): no check passes for a thing that never
// ran, and the run is partial rather than failed.
// Invariant: I-111, I-112
func TestSoak_MeshPhasesSkipWithoutMesh(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	for i := range c.Nodes {
		c.AddDisk(i)
	}
	env := &harnessEnv{t: t, c: c}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: soak.StandardPhases("25", "26")}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Phases) != 2 || sum.Failed != 0 || !sum.Partial {
		t.Fatalf("summary: %d phases, %d failed, partial %v", len(sum.Phases), sum.Failed, sum.Partial)
	}
	for _, ph := range sum.Phases {
		if !ph.Skipped || ph.Reason == "" || len(ph.Checks) != 0 {
			t.Errorf("%s: skipped %v (%q), %d checks", ph.Name, ph.Skipped, ph.Reason, len(ph.Checks))
		}
	}
}
