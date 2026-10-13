package soak

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// evidenceEnv is the fake environment able to save evidence; it records where it was asked to.
type evidenceEnv struct {
	*fakeEnv
	mu   sync.Mutex
	dirs []string
}

func (e *evidenceEnv) TakeEvidence(_ context.Context, dir string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dirs = append(e.dirs, dir)
	return nil
}

// The first failed check of a phase saves the rig's evidence (each node's recent log and
// status) into a folder of its own under EvidenceDir, once a phase, and the results name
// that folder; a phase without a failure saves nothing. Before, the compose rig was torn
// down with its logs, and a failure the report could not explain stayed unexplained.
// Invariant: I-270
func TestExecute_FailureSavesEvidence(t *testing.T) {
	e := &evidenceEnv{fakeEnv: newFakeEnv(1)}
	var out bytes.Buffer
	failing := func(name string, n int) Phase {
		return Phase{Name: name, Run: func(_ context.Context, r *Run) error {
			for range n {
				r.check("I-1", "it held", false, "it did not")
			}
			return nil
		}}
	}
	r := fakeRun(e.fakeEnv, &out, failing("12 large file", 2), okPhase("13 disks"), failing("22 copies under faults", 1))
	r.Env = e
	r.EvidenceDir = t.TempDir()
	if _, err := r.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(r.EvidenceDir, "12-large-file"), filepath.Join(r.EvidenceDir, "22-copies-under-faults")}
	if !slices.Equal(e.dirs, want) {
		t.Fatalf("evidence saved to %v, want %v (once per failing phase)", e.dirs, want)
	}
	for _, d := range want {
		if !strings.Contains(out.String(), d) {
			t.Errorf("the results do not name %s", d)
		}
	}

	// without a folder, or with an environment that cannot, nothing is asked
	e2 := &evidenceEnv{fakeEnv: newFakeEnv(1)}
	r2 := fakeRun(e2.fakeEnv, &bytes.Buffer{}, failing("1 x", 1))
	r2.Env = e2
	if _, err := r2.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e2.dirs) != 0 {
		t.Fatalf("evidence saved with no EvidenceDir: %v", e2.dirs)
	}
}
