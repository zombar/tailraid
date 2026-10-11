package soak

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/repair"
)

// slowVolume gives every whole-file read the same floor, so the idle and the
// during-the-pass reads of the fake are comparable rather than noise.
type slowVolume struct {
	Volume
	delay time.Duration
}

func (v slowVolume) Hash(p string) ([32]byte, int64, error) {
	<-time.After(v.delay)
	return v.Volume.Hash(p)
}

// runFanOut drives phase 18 on a fake whose first repair pass reports peak chunks in
// flight of workers for the given time; later passes find nothing to do.
func runFanOut(t *testing.T, workers, peak int, pass time.Duration, mutate bool) ([]PhaseResult, string) {
	t.Helper()
	e := newFakeEnv(2)
	e.caps = Caps{MultiGateway: true, Mutate: mutate}
	e.gateway = &admin.GatewayReport{}
	e.fs = slowVolume{Volume: e.fs, delay: 30 * time.Millisecond}
	calls := 0
	e.pass = func(ctx context.Context) (repair.Stats, error) {
		e.mu.Lock()
		calls++
		first := calls == 1
		if first {
			e.repair = &admin.RepairStatus{InProgress: &repair.Progress{Pass: 1, Total: fanoutChunks, InFlight: peak, Workers: workers}}
		}
		e.mu.Unlock()
		if !first {
			return repair.Stats{}, nil // the migration back: nothing left
		}
		select {
		case <-time.After(pass):
		case <-ctx.Done():
		}
		e.mu.Lock()
		e.repair = &admin.RepairStatus{}
		e.mu.Unlock()
		return repair.Stats{Scanned: fanoutChunks, Migrated: fanoutChunks}, nil
	}
	var out bytes.Buffer
	r := fakeRun(e, &out, Phase{Name: "18 repair fan-out", Run: phaseFanOut})
	sum, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum.Phases, out.String()
}

// A pass whose in-flight peak is its worker count passes every check: the bound, the
// overlap, the cold read during the pass, the content and the migration back; the pass
// rate and the two reads are events.
// Invariant: I-174, I-175
func TestPhaseFanOut_ConformingPassPasses(t *testing.T) {
	phases, out := runFanOut(t, 4, 4, 600*time.Millisecond, true)
	if len(phases) != 1 || phases[0].Failed != 0 || phases[0].Skipped {
		t.Fatalf("phase: %+v\n%s", phases, out)
	}
	for _, name := range []string{"repairs in flight within", "the pass ran chunks", "a cold read during the pass", "migration back"} {
		if ok, _ := checksNamed(phases[0], name); ok != 1 {
			t.Fatalf("%q not passed:\n%s", name, out)
		}
	}
	for _, ev := range []string{"repair pass", "cold read idle", "cold read during the pass"} {
		if !strings.Contains(out, ev) {
			t.Fatalf("no %q event:\n%s", ev, out)
		}
	}
}

// A pass that reports more chunks in flight than its workers fails the bound check and
// nothing else.
// Invariant: I-174
func TestPhaseFanOut_OverWorkersFails(t *testing.T) {
	phases, out := runFanOut(t, 4, 5, 600*time.Millisecond, true)
	if len(phases) != 1 || phases[0].Failed != 1 {
		t.Fatalf("phase: %+v\n%s", phases, out)
	}
	if ok, skipped := checksNamed(phases[0], "repairs in flight within"); ok != 0 || skipped != 0 {
		t.Fatalf("bound check ok %d skipped %d:\n%s", ok, skipped, out)
	}
}

// A pass that ends before a status sample saw it running skips the in-flight and the
// during-the-pass checks (reported, I-113), never passes them.
// Invariant: I-174
func TestPhaseFanOut_FastPassSkips(t *testing.T) {
	phases, out := runFanOut(t, 4, 4, 0, true)
	if len(phases) != 1 || phases[0].Failed != 0 {
		t.Fatalf("phase: %+v\n%s", phases, out)
	}
	for _, name := range []string{"repairs in flight within", "the pass ran chunks", "a cold read during the pass"} {
		if ok, skipped := checksNamed(phases[0], name); ok != 0 || skipped != 1 {
			t.Fatalf("%q ok %d skipped %d:\n%s", name, ok, skipped, out)
		}
	}
}

// Without Caps.Mutate the phase is skipped whole: its backlog is a policy change.
// Invariant: I-112
func TestPhaseFanOut_NeedsMutate(t *testing.T) {
	phases, out := runFanOut(t, 4, 4, 0, false)
	if len(phases) != 1 || !phases[0].Skipped {
		t.Fatalf("phase: %+v\n%s", phases, out)
	}
}
