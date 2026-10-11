package admin

import (
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// Reporter keeps the status report off the request path (docs/12 §8, I-205): building
// one walks every chunk (ScanHealth), so a request never runs the walk itself. The first
// Get builds synchronously; from then on a refresher rebuilds the report every
// ReportEvery for as long as someone asked for it within ReportIdle, and every Get
// returns the latest build at once. The report's GeneratedAt says how old it is.
type Reporter struct {
	build func() Report
	clk   clock.Clock
	every time.Duration
	idle  time.Duration

	mu      sync.Mutex
	rep     Report
	have    bool
	first   chan struct{} // closed once the first build is stored; later first-callers wait on it
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

// NewReporter wraps build, which is called on the reporter's goroutine or, once, on the
// first caller's.
func NewReporter(build func() Report, clk clock.Clock) *Reporter {
	return &Reporter{build: build, clk: clk, every: ReportEvery, idle: ReportIdle, first: make(chan struct{}), stop: make(chan struct{})}
}

// Get returns the latest report. Only the very first call builds one on the caller's
// goroutine (a concurrent first caller waits for that build rather than starting another).
func (r *Reporter) Get() Report {
	r.mu.Lock()
	r.asked = r.clk.Now()
	if !r.have {
		first := r.first
		building := r.running // the first caller marks itself running while it builds
		if !building {
			r.running = true
			r.mu.Unlock()
			rep := r.build()
			r.mu.Lock()
			r.rep, r.have = rep, true
			close(first)
			r.startLocked()
		} else {
			r.mu.Unlock()
			<-first
			r.mu.Lock()
		}
	} else if !r.running {
		r.startLocked()
	}
	rep := r.rep
	r.mu.Unlock()
	return rep
}

// Running reports whether the refresher is up (tests).
func (r *Reporter) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// startLocked starts the refresher unless the reporter is closed. The caller holds mu.
func (r *Reporter) startLocked() {
	r.running = !r.closed
	if r.closed {
		return
	}
	r.done = make(chan struct{})
	go r.refresh(r.done, r.clk.NewTimer(r.every)) // armed here, so a clock advanced right after Get fires it
}

func (r *Reporter) refresh(done chan struct{}, t clock.Timer) {
	defer close(done)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
			return
		case <-t.C():
		}
		r.mu.Lock()
		if r.clk.Now().Sub(r.asked) > r.idle || r.closed { // nobody is looking: stop until the next Get
			r.running = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		rep := r.build()
		r.mu.Lock()
		r.rep = rep
		r.mu.Unlock()
		t.Reset(r.every)
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
