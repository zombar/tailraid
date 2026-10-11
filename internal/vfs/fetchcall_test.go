package vfs

import "testing"

// A reader that reaches a fetch between its finish and the moment it leaves the in-flight
// table must not wait on a slot nobody will close: after finish every slot reads closed,
// a publish is a no-op, and the shards published before stay readable. (Found by the
// harness soak: phase 12's hash read hung 10 minutes on exactly this window.)
// Invariant: I-202
func TestFetchCall_SlotAfterFinishIsClosed(t *testing.T) {
	c := &fetchCall{done: make(chan struct{})}
	c.publish(2, 0, []byte("shard 0"))
	c.finish()
	for j := range 2 {
		select {
		case <-c.slot(2, j):
		default:
			t.Fatalf("slot %d still open after finish", j)
		}
	}
	c.publish(2, 1, []byte("late")) // must not panic or reopen anything
	if got := c.shard(0); string(got) != "shard 0" {
		t.Fatalf("shard 0 after finish: %q", got)
	}
	if got := c.shard(1); got != nil {
		t.Fatalf("a shard published after finish was kept: %q", got)
	}
	// a call finished before anything was armed: a late slot is closed too
	late := &fetchCall{done: make(chan struct{})}
	late.finish()
	select {
	case <-late.slot(3, 2):
	default:
		t.Fatal("slot of a never-armed finished call is open")
	}
}
