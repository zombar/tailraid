package main

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/rpc"
)

// `tailraid status` prints one line per unfinished restore job: where it lands and runs,
// its state, how far it got, the rate and the time left while it runs, and why it waits or
// retries (docs/12 §11).
// Invariant: I-365
func TestStatus_RestoreLine(t *testing.T) {
	for _, c := range []struct {
		j    rpc.RestoreJob
		want string
	}{
		{rpc.RestoreJob{ID: "3-1", Dst: "/music", Host: "honker", State: rpc.RestoreCounting}, "restore 3-1  /music on honker · counting"},
		{
			rpc.RestoreJob{ID: "3-7", Dst: "/music", Host: "honker", State: rpc.RestoreRunning, Counted: true, BytesDone: 280 << 30, BytesTotal: 895 << 30, Rate: 41 << 20},
			"restore 3-7  /music on honker · running · 280.0 GiB of 895.0 GiB · 41.0 MiB/s · 4 h 16 m left",
		},
		{
			rpc.RestoreJob{ID: "3-8", Dst: "/films", Host: "honker", State: rpc.RestoreRetrying, Counted: true, BytesDone: 1 << 30, BytesTotal: 2 << 30, Error: "no leader"},
			"restore 3-8  /films on honker · retrying · 1.0 GiB of 2.0 GiB · no leader",
		},
	} {
		if got := restoreLine(&c.j); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	for d, want := range map[int64]string{40: "40 s", 12 * 60: "12 m", 4*3600 + 16*60: "4 h 16 m"} {
		if got := leftStr(timeSeconds(d)); got != want {
			t.Errorf("leftStr(%d s) = %q, want %q", d, got, want)
		}
	}
}

func timeSeconds(s int64) time.Duration { return time.Duration(s) * time.Second }
