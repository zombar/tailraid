package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// IdleWait bounds how `update --restart` waits for an idle gateway (docs/12 §5, I-221).
type IdleWait struct {
	IdleFor time.Duration // how long the reports must stay quiet without a break
	Poll    time.Duration // between two reports
	Max     time.Duration // then the restart goes ahead anyway
}

// DefaultIdleWait is update's: quiet for 10 s, a report every 2 s, at most 10 minutes.
var DefaultIdleWait = IdleWait{IdleFor: 10 * time.Second, Poll: 2 * time.Second, Max: 10 * time.Minute}

// WaitIdle reads the node's status every Poll until it has shown, for IdleFor without a
// break, no dirty, plan or WAL bytes and no growth in the bytes clients wrote: a restart
// then drops no client's copy (I-221). It reports true then, or at once when the status
// cannot be read (no running gateway, nothing to protect), and false after Max. note
// hears what it is waiting for whenever that changes.
func WaitIdle(ctx context.Context, clk clock.Clock, w IdleWait, fetch func(context.Context) (Report, error), note func(string)) (bool, error) {
	start := clk.Now()
	var (
		since    time.Time // the start of the current quiet run; zero: not quiet
		written  uint64
		haveLast bool
		said     string
	)
	for {
		rep, err := fetch(ctx)
		if err != nil {
			return true, nil //nolint:nilerr // an unreadable status is no running gateway: nothing to protect (I-221)
		}
		var g GatewayReport
		if rep.Gateway != nil {
			g = *rep.Gateway
		}
		now := clk.Now()
		why := ""
		switch {
		case g.DirtyBytes > 0 || g.PlanBytes > 0 || g.WALBytes > 0:
			why = fmt.Sprintf("%d bytes not yet flushed", g.DirtyBytes+g.PlanBytes+uint64(max(g.WALBytes, 0)))
		case haveLast && g.BytesWritten != written:
			why = "clients are writing"
		}
		written, haveLast = g.BytesWritten, true
		if why != "" {
			since = time.Time{}
			if why != said {
				note("waiting for the gateway to go idle: " + why)
				said = why
			}
		} else if since.IsZero() {
			since = now
		}
		if !since.IsZero() && now.Sub(since) >= w.IdleFor {
			return true, nil
		}
		if now.Sub(start) >= w.Max {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-clk.After(w.Poll):
		}
	}
}
