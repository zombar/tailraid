package soak

import (
	"bytes"
	"strings"
	"testing"
)

// Without a sampler the phase is skipped for the capability, never passed or failed.
// Invariant: I-114
func TestPhaseStats_SkipsWithoutSampler(t *testing.T) {
	e := newFakeEnv(3)
	var out bytes.Buffer
	r := fakeRun(e, &out, Phase{Name: "20 stats", Run: phaseStats})
	sum, err := r.Execute(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Phases) != 1 || !sum.Phases[0].Skipped || !strings.Contains(sum.Phases[0].Reason, "sampler") {
		t.Fatalf("phase %+v", sum.Phases[0])
	}
}
