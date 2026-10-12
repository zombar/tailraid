package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// runWaitIdle runs WaitIdle against status(t), t the time since the start, advancing the
// fake clock one poll whenever WaitIdle is parked on it; it returns the answer and when.
func runWaitIdle(t *testing.T, w IdleWait, status func(time.Duration) (GatewayReport, error)) (bool, time.Duration) {
	t.Helper()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	start := clk.Now()
	type result struct {
		idle bool
		err  error
		at   time.Duration
	}
	done := make(chan result, 1)
	go func() {
		idle, err := WaitIdle(context.Background(), clk, w, func(context.Context) (Report, error) {
			g, err := status(clk.Now().Sub(start))
			return Report{Gateway: &g}, err
		}, func(string) {})
		done <- result{idle, err, clk.Now().Sub(start)}
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			return r.idle, r.at
		case <-deadline:
			t.Fatal("WaitIdle never returned")
		default:
		}
		if clk.Pending() > 0 {
			clk.Advance(w.Poll)
		}
	}
}

// update waits for IdleFor of reports showing nothing dirty and no new client bytes; a
// report that breaks the run restarts the count; after Max it gives up and says so.
// Invariant: I-221
func TestWaitIdle_Table(t *testing.T) {
	w := IdleWait{IdleFor: 10 * time.Second, Poll: 2 * time.Second, Max: time.Minute}
	quiet := GatewayReport{BytesWritten: 500}
	for _, tc := range []struct {
		name     string
		status   func(time.Duration) GatewayReport
		wantIdle bool
		wantAt   time.Duration
	}{
		{"idle from the start", func(time.Duration) GatewayReport { return quiet }, true, 10 * time.Second},
		{"writing until 20 s", func(at time.Duration) GatewayReport {
			if at < 20*time.Second {
				return GatewayReport{BytesWritten: uint64(at)}
			}
			return GatewayReport{BytesWritten: uint64(20 * time.Second)}
		}, true, 32 * time.Second}, // the last growth is seen at 20 s, the first quiet report at 22 s
		{"dirty once at 6 s", func(at time.Duration) GatewayReport {
			if at == 6*time.Second {
				return GatewayReport{BytesWritten: 500, DirtyBytes: 1}
			}
			return quiet
		}, true, 18 * time.Second},
		{"a write two seconds short of IdleFor", func(at time.Duration) GatewayReport {
			if at >= 8*time.Second {
				return GatewayReport{BytesWritten: 501}
			}
			return quiet
		}, true, 20 * time.Second},
		{"a WAL that never drains", func(time.Duration) GatewayReport { return GatewayReport{WALBytes: 1} }, false, time.Minute},
		{"plan bytes that never drain", func(time.Duration) GatewayReport { return GatewayReport{PlanBytes: 1} }, false, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idle, at := runWaitIdle(t, w, func(at time.Duration) (GatewayReport, error) { return tc.status(at), nil })
			if idle != tc.wantIdle || at != tc.wantAt {
				t.Fatalf("idle %v at %v, want %v at %v", idle, at, tc.wantIdle, tc.wantAt)
			}
		})
	}
}

// A node whose status cannot be read has no gateway to protect: the restart goes ahead at
// once.
// Invariant: I-221
func TestWaitIdle_StatusUnreadable_ReturnsAtOnce(t *testing.T) {
	w := IdleWait{IdleFor: 10 * time.Second, Poll: 2 * time.Second, Max: time.Minute}
	idle, at := runWaitIdle(t, w, func(time.Duration) (GatewayReport, error) { return GatewayReport{}, errors.New("connection refused") })
	if !idle || at != 0 {
		t.Fatalf("idle %v at %v, want true at once", idle, at)
	}
}
