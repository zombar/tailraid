package vfs_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// A PUT that never answers counts as a failure once the deadline passes: the flush goes
// on with the k acks it has, the straggler's slot is re-placed on a spare node, and the
// commit lands with a full placement that avoids the stalled node.
// Lifecycle: L-43, L-41
func TestFlusher_PutTimeout_SlotReplaced(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 7)
	must(t, f.Ov.Write(ino, 0, 0, data))
	h := sha256Of(data)
	cands := meta.Candidates(f.FSM.Nodes(), func(meta.NodeIdx) bool { return true })
	victim := meta.PlaceKM(h, cands, 3, 0)[2]
	entered, release := f.StallPut(victim)
	defer release()
	cfg := vfs.DefaultConfig()
	cfg.PutDeadline = time.Second
	fl := f.Flusher(func(c *vfs.Config) { *c = cfg })
	done := make(chan error, 1)
	go func() { done <- fl.FlushInode(ctx, ino) }()
	<-entered
	var err error
	// The deadline is on the fake clock. It is advanced in small steps (every 2 ms of
	// real time) so the PUTs that do answer, including the re-placed one, land well
	// inside their own deadline while the stalled one runs out of it.
	// The deadline is on the fake clock. It is advanced until the stalled PUT has been
	// cancelled, which is the first round ending; the re-placed PUT then runs under a
	// deadline nothing advances, so it can never run out of time however loaded the
	// machine is.
	testharness.Eventually(t, func() bool {
		if f.CancelledPuts(victim) == 0 {
			f.Clk.Advance(cfg.PutDeadline / 4)
		}
		select {
		case err = <-done:
			return true
		default:
			return false
		}
	}, "flush past the PUT deadline")
	must(t, err)
	release() // let the stalled PUT finish before the test's temp dir goes away
	c, ok := f.FSM.GetChunk(h)
	if !ok {
		t.Fatal("chunk not committed")
	}
	for _, s := range c.Placement {
		if s == 0 || s == victim {
			t.Fatalf("placement %v should be full and avoid the stalled node %d", c.Placement, victim)
		}
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
}

// A node that refuses a PUT with 403 (membership mismatch) is treated like any failed
// slot: re-placed on a spare, the refusal recorded in the chunk's history, the commit
// unaffected.
// Lifecycle: L-46
func TestFlusher_PutForbidden_SlotReplaced(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 8)
	must(t, f.Ov.Write(ino, 0, 0, data))
	h := sha256Of(data)
	cands := meta.Candidates(f.FSM.Nodes(), func(meta.NodeIdx) bool { return true })
	victim := meta.PlaceKM(h, cands, 3, 0)[0]
	f.FailPutWith(victim, rpc.ErrForbidden)
	fl := f.Flusher()
	fl.Events = meta.NewEventRing(f.Clk, f.SelfIdx)
	must(t, fl.FlushInode(ctx, ino))
	c, _ := f.FSM.GetChunk(h)
	for _, s := range c.Placement {
		if s == 0 || s == victim {
			t.Fatalf("placement %v should be full and avoid the refusing node %d", c.Placement, victim)
		}
	}
	failed := false
	for _, k := range fl.Events.Kinds(h) {
		failed = failed || k == meta.EvPutFailed
	}
	if !failed {
		t.Fatalf("no put_failed event: %v", fl.Events.Kinds(h))
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
}
