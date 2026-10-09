package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// An already-expired context is refused before dispatch (raft would otherwise wait with
// no timer); a context cancelled while the future is pending reports an unknown outcome,
// since the entry may still commit. Neither is ever reported as "not the leader".
//
// Invariant: I-95
func TestApplyLocal_ExpiredAndCancelledContext(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	cmd := meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: 1, Name: "d", Mode: 0o755}}

	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	start := time.Now()
	_, _, err := n.Propose(expired, cmd)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotLeader) {
		t.Fatalf("expired context: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("an expired context waited %s for raft", time.Since(start))
	}
	if _, ok := n.FSM.Lookup(1, "d"); ok {
		t.Fatal("an expired proposal was applied")
	}

	// a live context: applied, and a repeat is the FSM's own answer (ErrExist), not ours
	if res, _, err := n.Propose(ctx, cmd); err != nil || res.Err != nil {
		t.Fatalf("live proposal: %v %v", err, res.Err)
	}
	if res, _, err := n.Propose(ctx, cmd); err != nil || !errors.Is(res.Err, meta.ErrExist) {
		t.Fatalf("repeat: %v %v", err, res.Err)
	}
}
