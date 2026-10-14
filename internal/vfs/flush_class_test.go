package vfs_test

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// The straggler grace follows the p95 of PUTs of the straggler's own path class: a slow
// WAN member's PUTs do not stretch the grace of a LAN straggler, and a WAN straggler keeps
// its class's longer grace.
// Invariant: I-327, I-169
func TestFlusher_StragglerGrace_PerClass(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1) // every node holds a slot: 1 is self, 2 a LAN peer, 3 a WAN peer
	const lan, wan = meta.NodeIdx(2), meta.NodeIdx(3)
	f.SetClass(wan, meta.PathWAN)
	cfg := vfs.DefaultConfig()
	cfg.PutDeadline, cfg.StragglerMin = 100*time.Second, 2*time.Second
	fl := f.Flusher(func(c *vfs.Config) { *c = cfg })
	step := cfg.StragglerMin / 4
	// flush writes one chunk with node's PUT stalled, and once k acks are in advances the
	// clock by `hold` before releasing the stall (slow is the PUT's measured duration),
	// or steps it until the flush ends when hold is 0; it returns the clock advanced.
	flush := func(name string, node meta.NodeIdx, hold time.Duration, seed byte) time.Duration {
		t.Helper()
		ino := f.Create(name)
		data := testharness.Pattern(int(testharness.ChunkSize), seed)
		must(t, f.Ov.Write(ino, 0, 0, data))
		h := sha256Of(data)
		entered, release := f.StallPut(node)
		defer release()
		timers := f.Clk.Pending()
		done := make(chan error, 1)
		go func() { done <- fl.FlushInode(ctx, ino) }()
		<-entered
		// the k-th ack arms the grace timer beside the deadline's: only then is the clock
		// advanced, so the live PUTs are measured at zero and the stall alone takes time
		testharness.Eventually(t, func() bool { return len(f.ShardsOn(h, 2, 1)) == 2 && f.Clk.Pending() >= timers+2 }, "the grace was never armed")
		var advanced time.Duration
		if hold > 0 {
			f.Clk.Advance(hold)
			release()
			must(t, <-done)
			return hold
		}
		var err error
		testharness.Eventually(t, func() bool {
			if f.CancelledPuts(node) == 0 {
				f.Clk.Advance(step)
				advanced += step
			}
			select {
			case err = <-done:
				return true
			default:
				return false
			}
		}, "flush past the straggler grace")
		must(t, err)
		return advanced
	}
	slow := 1500 * time.Millisecond // under the floor, so the PUT completes and its duration is recorded
	flush("a", wan, slow, 1)        // the WAN ring's p95 is now 1.5 s: a WAN straggler's grace is 6 s
	if got := flush("b", lan, 0, 2); got > 4*slow-step {
		t.Fatalf("a LAN straggler was cut after %s: the WAN p95 stretched its grace (want about the %s floor)", got, cfg.StragglerMin)
	} else if got < cfg.StragglerMin-step {
		t.Fatalf("a LAN straggler was cut after %s, before the %s floor", got, cfg.StragglerMin)
	}
	if got := flush("c", wan, 0, 3); got < 4*slow-step || got > 4*slow+2*step {
		t.Fatalf("a WAN straggler was cut after %s, want about 4 × %s", got, slow)
	}
}
