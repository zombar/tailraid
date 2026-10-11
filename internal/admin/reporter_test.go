package admin_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/testharness"
)

// countingBuild numbers its reports and can hold one build until released.
type countingBuild struct {
	n     atomic.Int64
	hold  chan struct{} // nil: never hold
	holdN int64         // the build number that blocks on hold
}

func (b *countingBuild) build() admin.Report {
	n := b.n.Add(1)
	if b.hold != nil && n == b.holdN {
		<-b.hold
	}
	return admin.Report{Node: "n", WalkMS: n}
}

// Only the first request builds the report on its own goroutine; every later request
// returns the latest build at once, even while a rebuild is under way, and the rebuild
// lands for the request after it.
// Invariant: I-205
func TestReporter_RequestNeverWalks(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := &countingBuild{hold: make(chan struct{}), holdN: 2}
	r := admin.NewReporter(b.build, fc)
	defer r.Close()
	if got := r.Get().WalkMS; got != 1 {
		t.Fatalf("first report %d, want build 1", got)
	}
	if got := r.Get().WalkMS; got != 1 || b.n.Load() != 1 {
		t.Fatalf("second request: report %d after %d builds; it must not build", got, b.n.Load())
	}
	fc.Advance(admin.ReportEvery) // the refresher starts build 2, which blocks
	testharness.Eventually(t, func() bool { return b.n.Load() == 2 }, "the refresher builds")
	served := make(chan int64, 1)
	go func() { served <- r.Get().WalkMS }()
	select {
	case got := <-served:
		if got != 1 {
			t.Fatalf("request during a rebuild got %d, want the previous report", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request waited on the rebuild")
	}
	close(b.hold)
	testharness.Eventually(t, func() bool { return r.Get().WalkMS == 2 }, "the rebuilt report is served")
}

// The refresher runs only while the report is wanted: with no request for ReportIdle it
// stops, and the next request restarts it.
// Invariant: I-205
func TestReporter_IdleStopsRefreshing(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := &countingBuild{}
	r := admin.NewReporter(b.build, fc)
	defer r.Close()
	r.Get()
	for i := range 3 {
		fc.Advance(admin.ReportEvery)
		want := int64(2 + i)
		testharness.Eventually(t, func() bool { return b.n.Load() == want && fc.Pending() == 1 }, "a rebuild per tick while wanted, then the timer re-armed")
		r.Get()
	}
	fc.Advance(admin.ReportIdle + admin.ReportEvery) // nobody asked since: the refresher leaves
	testharness.Eventually(t, func() bool { return !r.Running() }, "the refresher stops when idle")
	builds := b.n.Load()
	fc.Advance(10 * admin.ReportEvery)
	if b.n.Load() != builds {
		t.Fatalf("%d builds while idle, want none after %d", b.n.Load()-builds, builds)
	}
	r.Get() // wanted again
	testharness.Eventually(t, func() bool { return r.Running() && fc.Pending() == 1 }, "the refresher restarts on a request")
	fc.Advance(admin.ReportEvery)
	testharness.Eventually(t, func() bool { return b.n.Load() == builds+1 }, "it rebuilds again")
}

// Close stops the refresher; a request after it serves the last report and starts nothing.
// Invariant: I-205
func TestReporter_CloseStopsRefresher(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := &countingBuild{}
	r := admin.NewReporter(b.build, fc)
	r.Get()
	r.Close()
	if r.Running() {
		t.Fatal("refresher running after Close")
	}
	fc.Advance(5 * admin.ReportEvery)
	if got := r.Get().WalkMS; got != 1 || b.n.Load() != 1 {
		t.Fatalf("after Close: report %d, %d builds", got, b.n.Load())
	}
	if r.Running() {
		t.Fatal("a request after Close restarted the refresher")
	}
}
