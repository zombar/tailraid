// Package clock is the time seam used by every tailraid component that waits, expires or
// measures. Production code uses Real(); tests use a Fake whose time only moves when the
// test says so, which removes every time.Sleep from the test suite.
package clock

import (
	"sync"
	"time"
)

// Clock is the subset of package time that tailraid depends on.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	After(d time.Duration) <-chan time.Time
	Since(t time.Time) time.Duration
}

// Timer mirrors *time.Timer behind an interface so it can be faked.
type Timer interface {
	// C delivers the fire time once; a stopped or already-fired timer never delivers.
	C() <-chan time.Time
	// Stop reports whether the timer was pending.
	Stop() bool
	// Reset re-arms the timer for d from now and reports whether it was pending.
	Reset(d time.Duration) bool
}

// Real returns the wall clock.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (realClock) NewTimer(d time.Duration) Timer         { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

// Fake is a deterministic Clock. Time advances only through Advance or Set; timers whose
// deadline is reached fire in deadline order (ties in creation order) during that call.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers map[uint64]*fakeTimer
	log    []uint64
}

// NewFake returns a Fake positioned at start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start, timers: map[uint64]*fakeTimer{}}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

func (f *Fake) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	t := &fakeTimer{f: f, id: f.seq, ch: make(chan time.Time, 1)}
	f.armLocked(t, d)
	return t
}

// Pending is the number of armed timers: what is waiting on this clock right now.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// Advance moves the clock forward by d, firing every timer due at or before the new time.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	f.mu.Unlock()
	f.Set(target)
}

// Set moves the clock to t (forward or backward); timers due at or before t fire.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		var next *fakeTimer
		for _, tm := range f.timers {
			if tm.deadline.After(t) {
				continue
			}
			if next == nil || tm.deadline.Before(next.deadline) ||
				(tm.deadline.Equal(next.deadline) && tm.id < next.id) {
				next = tm
			}
		}
		if next == nil {
			break
		}
		if next.deadline.After(f.now) {
			f.now = next.deadline
		}
		delete(f.timers, next.id)
		next.pending = false
		next.ch <- next.deadline
		f.log = append(f.log, next.id)
	}
	if t.After(f.now) || t.Before(f.now) {
		f.now = t
	}
}

func (f *Fake) armLocked(t *fakeTimer, d time.Duration) {
	t.deadline = f.now.Add(d)
	t.pending = true
	f.timers[t.id] = t
	if d <= 0 {
		delete(f.timers, t.id)
		t.pending = false
		t.ch <- t.deadline
		f.log = append(f.log, t.id)
	}
}

// fireLog returns the creation sequence numbers of fired timers, in firing order.
func (f *Fake) fireLog() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint64, len(f.log))
	copy(out, f.log)
	return out
}

type fakeTimer struct {
	f        *Fake
	id       uint64
	ch       chan time.Time
	deadline time.Time
	pending  bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	was := t.pending
	delete(t.f.timers, t.id)
	t.pending = false
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	was := t.pending
	// Like time.Timer.Reset, a value left in the channel from a previous fire stays there;
	// callers that reuse a fired timer drain C first.
	t.f.armLocked(t, d)
	return was
}
