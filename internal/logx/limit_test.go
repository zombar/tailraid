package logx

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// One line per interval; the next line carries how many were swallowed; the count
// resets after it is reported; a fresh limiter logs at once.
//
// Invariant: I-124
func TestLimiter_OneLinePerInterval(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	l := &Limiter{Every: time.Minute, Clk: clk}
	l.Warn(log, "flush failed", "ino", 1)
	for i := range 5 {
		clk.Advance(10 * time.Second)
		l.Warn(log, "flush failed", "ino", i+2)
	}
	if n := strings.Count(out.String(), "flush failed"); n != 1 {
		t.Fatalf("%d lines within the interval, want 1:\n%s", n, out.String())
	}
	clk.Advance(10 * time.Second) // 60 s since the first line
	l.Warn(log, "flush failed", "ino", 9)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "suppressed=5") || !strings.Contains(lines[1], "ino=9") {
		t.Fatalf("second line: %v", lines)
	}
	clk.Advance(time.Minute)
	l.Error(log, "flush failed", "ino", 10)
	lines = strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || strings.Contains(lines[2], "suppressed") || !strings.Contains(lines[2], "level=ERROR") {
		t.Fatalf("third line: %v", lines)
	}
	// the zero value works on the wall clock with the default interval
	var z Limiter
	out.Reset()
	z.Warn(log, "x")
	z.Warn(log, "x")
	if n := strings.Count(out.String(), "msg=x"); n != 1 {
		t.Fatalf("zero value: %d lines", n)
	}
}
