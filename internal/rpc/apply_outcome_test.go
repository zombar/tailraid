package rpc

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// A leader whose Propose fails after dispatch answers 500, never 421 or 503: the caller
// must not take "not the leader" (safe to retry elsewhere) for "outcome unknown".
//
// Invariant: I-95
func TestApply_UnknownOutcomeIs500(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	cmd := meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: 1, Name: "d", Mode: 0o755}}
	e.app.mu.Lock()
	e.app.proposeErr = errors.New("raft: leadership lost while committing log")
	e.app.mu.Unlock()
	_, _, err := c.ApplyAt(ctx, e.addr, cmd)
	switch {
	case err == nil:
		t.Fatal("a lost outcome was reported as success")
	case errors.Is(err, ErrNotLeader), errors.Is(err, ErrNoLeader):
		t.Fatalf("a lost outcome was reported as a leader error: %v", err)
	case !errors.Is(err, ErrApplyFailed):
		t.Fatalf("want ErrApplyFailed, got %v", err)
	}
	// the command did reach the FSM: a blind retry would find the directory there
	if _, ok := e.app.fsm.Lookup(1, "d"); !ok {
		t.Fatal("the fake did not apply before failing")
	}
	e.app.mu.Lock()
	e.app.proposeErr = nil
	e.app.mu.Unlock()
	e.app.setLeader(false, "100.64.0.9:20481")
	if _, _, err := c.ApplyAt(ctx, e.addr, cmd); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a follower still answers not-leader: %v", err)
	}
}
