package rpc

import "testing"

// /health carries ready: true while the applier knows a leader, false once it does not.
//
// Invariant: I-125
func TestHealth_Ready(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	h, err := c.Health(ctx, e.addr)
	if err != nil || !h.Ready {
		t.Fatalf("leader: ready=%v err=%v", h.Ready, err)
	}
	e.app.setLeader(false, "")
	if h, err = c.Health(ctx, e.addr); err != nil || h.Ready {
		t.Fatalf("no leader: ready=%v err=%v", h.Ready, err)
	}
	e.app.setLeader(false, "100.64.0.9:20481")
	if h, err = c.Health(ctx, e.addr); err != nil || !h.Ready {
		t.Fatalf("follower with a leader: ready=%v err=%v", h.Ready, err)
	}
}
