package restore

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// Clients are busy for YieldWindow after the host's last foreground shard request, and
// for YieldWindow after its gateways' client bytes last moved; quiet otherwise, the first
// look at the counters taking them as they are.
// Invariant: I-372
func TestClients_BusyWindow(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_000_000, 0))
	var fg time.Time
	var read, written uint64 = 500, 700 // a gateway that served clients before the job
	c := &Clients{LastForeground: func() time.Time { return fg }, ClientBytes: func() (uint64, uint64) { return read, written }, Clk: clk}
	if c.Busy() {
		t.Fatal("busy at the first look, with no foreground request and the bytes as they were")
	}
	fg = clk.Now()
	for _, tc := range []struct {
		after time.Duration
		busy  bool
	}{{0, true}, {YieldWindow - time.Nanosecond, true}, {YieldWindow, false}} {
		clk.Set(fg.Add(tc.after))
		if got := c.Busy(); got != tc.busy {
			t.Errorf("%v after a foreground request: busy %v, want %v", tc.after, got, tc.busy)
		}
	}
	moved := clk.Now()
	read += 4096 // a client read through this host's gateway
	if !c.Busy() {
		t.Fatal("not busy right after the client bytes moved")
	}
	clk.Set(moved.Add(YieldWindow - time.Nanosecond))
	if !c.Busy() {
		t.Fatal("not busy within YieldWindow of the bytes moving")
	}
	clk.Set(moved.Add(YieldWindow))
	if c.Busy() {
		t.Fatal("busy YieldWindow after the bytes last moved")
	}
}
