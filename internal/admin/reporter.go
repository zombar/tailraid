package admin

import (
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// Reporter keeps the status report off the request path (docs/12 §8, I-205): building
// one walks every chunk (ScanHealth), so a request never runs the walk itself. A refresher
// rebuilds the report every ReportEvery for as long as someone asked for it within
// ReportIdle, and a request while it runs returns the latest build at once. A request
// that finds it stopped, and a fresh one, wait for a build that starts after the request
// (I-214): a report from before an idle stretch, or from before the action a caller just
// took, is never served as current. The report's GeneratedAt says how old it is.
type Reporter struct {
	build func() Report
	clk   clock.Clock
	every time.Duration
	idle  time.Duration

	mu      sync.Mutex
	rep     Report
	started uint64        // builds begun
	stored  uint64        // the number of the build behind rep (0: none yet)
	need    uint64        // the latest build number a waiter needs
	landed  chan struct{} // closed and replaced whenever a build is stored
	kick    chan struct{} // wakes the refresher early for a waiter (buffered 1)
	asked   time.Time
	running bool
	closed  bool
	stop    chan struct{}
	done    chan struct{} // the refresher's exit
}

// ReportEvery is how often the refresher rebuilds the report while it is wanted;
// ReportIdle is how long after the last Get it stops.
const (
	ReportEvery = 2 * time.Second
	ReportIdle  = 30 * time.Second
)

// NewReporter wraps build, which is called on the reporter's goroutine (or, after Close
// with no report yet, on the caller's).
func NewReporter(build func() Report, clk clock.Clock) *Reporter {
	return &Reporter{
		build: build, clk: clk, every: ReportEvery, idle: ReportIdle,
		landed: make(chan struct{}), kick: make(chan struct{}, 1), stop: make(chan struct{}),
	}
}

// Get returns the status report. While the refresher runs, a plain request gets the
// latest build at once; a fresh request, or any request that finds the refresher
// stopped, waits for a build that begins after the request (I-214), and concurrent
// waiters share it.
func (r *Reporter) Get(fresh bool) Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = r.clk.Now()
	if r.closed {
		return r.lastLocked()
	}
	if r.stored > 0 && r.running && !fresh {
		return r.rep
	}
	need := r.started + 1 // the next build to begin; one under way began before this request
	r.need = max(r.need, need)
	if r.running {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	} else {
		r.startLocked()
	}
	for r.stored < need {
		landed := r.landed
		r.mu.Unlock()
		select {
		case <-landed:
			r.mu.Lock()
		case <-r.stop:
			r.mu.Lock()
			return r.lastLocked()
		}
	}
	return r.rep
}

// lastLocked is the report after Close: the last build, or one built on the caller's
// goroutine when there was none. The caller holds mu.
func (r *Reporter) lastLocked() Report {
	if r.stored == 0 {
		r.started++
		r.rep, r.stored = r.build(), r.started
	}
	return r.rep
}

// Running reports whether the refresher is up (tests).
func (r *Reporter) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// startLocked starts the refresher, which builds at once. The caller holds mu.
func (r *Reporter) startLocked() {
	r.running = true
	r.done = make(chan struct{})
	go r.refresh(r.done)
}

// refresh builds now and then every ReportEvery while the report is wanted, at once again
// when a waiter needs a build that begins after the one just stored.
func (r *Reporter) refresh(done chan struct{}) {
	defer close(done)
	var t clock.Timer
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	for {
		select { // a build beginning now serves every waiter that kicked before it
		case <-r.kick:
		default:
		}
		r.mu.Lock()
		r.started++
		n := r.started
		r.mu.Unlock()
		rep := r.build()
		r.mu.Lock()
		r.rep, r.stored = rep, n
		again := r.need > n // a waiter arrived after this build began
		if !again {
			// armed before the build is announced, so a clock advanced right after a Get
			// that waited for it fires the next one
			if t == nil {
				t = r.clk.NewTimer(r.every)
			} else {
				t.Reset(r.every)
			}
		}
		close(r.landed)
		r.landed = make(chan struct{})
		r.mu.Unlock()
		if again {
			continue
		}
		select {
		case <-r.stop:
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
			return
		case <-r.kick:
			continue
		case <-t.C():
		}
		r.mu.Lock()
		if r.clk.Now().Sub(r.asked) > r.idle && r.need <= r.stored { // nobody is looking: stop until the next Get
			r.running = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
	}
}

// Close stops the refresher and waits for it; a later Get serves the last report.
func (r *Reporter) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.stop)
	done := r.done
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}
