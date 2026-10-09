// Package logx holds the one logging helper the daemon needs beyond log/slog: a per-site
// rate limit for warnings that repeat per item (a flush that fails for every dirty inode,
// a repair that fails for every chunk, a refused web request per probe), so a fault
// produces one line a minute with a count instead of a flood (docs/12 §1, I-124).
package logx

import (
	"log/slog"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// DefaultEvery is the interval between two lines from one Limiter.
const DefaultEvery = time.Minute

// Limiter lets one warning through per Every and counts the ones it swallowed; the next
// line carries the count as "suppressed". The zero value limits to DefaultEvery on the
// wall clock.
type Limiter struct {
	Every time.Duration
	Clk   clock.Clock

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// Warn logs msg at Warn level unless a line went out within Every; args are slog
// key-value pairs.
func (l *Limiter) Warn(log *slog.Logger, msg string, args ...any) {
	if l.allow() {
		log.Warn(msg, l.args(args)...)
	}
}

// Error is Warn at Error level.
func (l *Limiter) Error(log *slog.Logger, msg string, args ...any) {
	if l.allow() {
		log.Error(msg, l.args(args)...)
	}
}

func (l *Limiter) allow() bool {
	every, clk := l.Every, l.Clk
	if every <= 0 {
		every = DefaultEvery
	}
	if clk == nil {
		clk = clock.Real()
	}
	now := clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < every {
		l.suppressed++
		return false
	}
	l.last = now
	return true
}

// args appends the suppressed count of the lines swallowed since the previous one.
func (l *Limiter) args(args []any) []any {
	l.mu.Lock()
	n := l.suppressed
	l.suppressed = 0
	l.mu.Unlock()
	if n > 0 {
		args = append(args, "suppressed", n)
	}
	return args
}
