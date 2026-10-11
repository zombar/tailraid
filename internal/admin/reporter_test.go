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
	if got := r.Get(false).WalkMS; got != 1 {
		t.Fatalf("first report %d, want build 1", got)
	}
	if got := r.Get(false).WalkMS; got != 1 || b.n.Load() != 1 {
		t.Fatalf("second request: report %d after %d builds; it must not build", got, b.n.Load())
	}
	fc.Advance(admin.ReportEvery) // the refresher starts build 2, which blocks
	testharness.Eventually(t, func() bool { return b.n.Load() == 2 }, "the refresher builds")
	served := make(chan int64, 1)
	go func() { served <- r.Get(false).WalkMS }()
	select {
	case got := <-served:
		if got != 1 {
			t.Fatalf("request during a rebuild got %d, want the previous report", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request waited on the rebuild")
	}
	close(b.hold)
	testharness.Eventually(t, func() bool { return r.Get(false).WalkMS == 2 }, "the rebuilt report is served")
}

// The refresher runs only while the report is wanted: with no request for ReportIdle it
// stops, and the next request restarts it.
// Invariant: I-205
func TestReporter_IdleStopsRefreshing(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := &countingBuild{}
	r := admin.NewReporter(b.build, fc)
	defer r.Close()
	r.Get(false)
	for i := range 3 {
		fc.Advance(admin.ReportEvery)
		want := int64(2 + i)
		testharness.Eventually(t, func() bool { return b.n.Load() == want && fc.Pending() == 1 }, "a rebuild per tick while wanted, then the timer re-armed")
		r.Get(false)
	}
	fc.Advance(admin.ReportIdle + admin.ReportEvery) // nobody asked since: the refresher leaves
	testharness.Eventually(t, func() bool { return !r.Running() }, "the refresher stops when idle")
	builds := b.n.Load()
	fc.Advance(10 * admin.ReportEvery)
	if b.n.Load() != builds {
		t.Fatalf("%d builds while idle, want none after %d", b.n.Load()-builds, builds)
	}
	r.Get(false) // wanted again: served by the build it restarted (I-214)
	testharness.Eventually(t, func() bool { return r.Running() && fc.Pending() == 1 }, "the refresher restarts on a request")
	fc.Advance(admin.ReportEvery)
	testharness.Eventually(t, func() bool { return b.n.Load() == builds+2 }, "it rebuilds again")
}

// Close stops the refresher; a request after it serves the last report and starts nothing.
// Invariant: I-205
func TestReporter_CloseStopsRefresher(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := &countingBuild{}
	r := admin.NewReporter(b.build, fc)
	r.Get(false)
	r.Close()
	if r.Running() {
		t.Fatal("refresher running after Close")
	}
	fc.Advance(5 * admin.ReportEvery)
	if got := r.Get(false).WalkMS; got != 1 || b.n.Load() != 1 {
		t.Fatalf("after Close: report %d, %d builds", got, b.n.Load())
	}
	if r.Running() {
		t.Fatal("a request after Close restarted the refresher")
	}
}

// A request that finds the refresher stopped is served by a build that started after it,
// never by the report from before the idle stretch (which may be hours old): fourteen soak
// checks read a report from before the action they followed.
// Invariant: I-214
func TestReporter_IdleRequestWaitsForFreshBuild(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := &countingBuild{}
	r := admin.NewReporter(b.build, fc)
	defer r.Close()
	r.Get(false)
	fc.Advance(admin.ReportIdle + admin.ReportEvery)
	testharness.Eventually(t, func() bool { return !r.Running() }, "the refresher stops when idle")
	before := b.n.Load()
	if got := r.Get(false).WalkMS; got <= before {
		t.Fatalf("a request after the idle stretch got build %d, want one after %d", got, before)
	}
}

// A fresh request waits for a build that starts after it, even while the refresher runs
// and a build is under way; concurrent fresh requests share one build; a plain request
// during the same wait still returns at once.
// Invariant: I-214
func TestReporter_FreshWaitsForNewBuild(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := &countingBuild{hold: make(chan struct{}), holdN: 2}
	r := admin.NewReporter(b.build, fc)
	defer r.Close()
	if got := r.Get(false).WalkMS; got != 1 {
		t.Fatalf("first report %d", got)
	}
	fc.Advance(admin.ReportEvery) // build 2 starts and blocks
	testharness.Eventually(t, func() bool { return b.n.Load() == 2 }, "the refresher builds")
	got := make(chan int64, 3)
	for range 3 {
		go func() { got <- r.Get(true).WalkMS }() // arrive while build 2 is under way
	}
	select {
	case n := <-got:
		t.Fatalf("a fresh request returned build %d before a build that started after it", n)
	case <-time.After(100 * time.Millisecond): // proving that nothing arrives
	}
	if plain := r.Get(false).WalkMS; plain != 1 {
		t.Fatalf("a plain request during the wait got %d, want the latest build 1 at once", plain)
	}
	close(b.hold) // build 2 lands; it started before the fresh requests, so build 3 serves them
	for range 3 {
		select {
		case n := <-got:
			if n < 3 {
				t.Fatalf("a fresh request got build %d, want one begun after it (3)", n)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a fresh request never returned")
		}
	}
	if n := b.n.Load(); n != 3 {
		t.Fatalf("%d builds for three concurrent fresh requests, want one shared (3 in all)", n)
	}
}
