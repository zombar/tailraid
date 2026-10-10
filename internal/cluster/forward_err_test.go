package cluster

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// A forward whose request never left (a refused dial, a fault rule) was not applied and
// is retried like a missing leader; one that may have reached the leader is an unknown
// outcome. The three-VM rounds saw a create fail with EIO through a gateway whose leader
// had just been cut off: two dropped forwards counted as two unknown outcomes.
// Invariant: I-95
func TestForwardErr_Table(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	dropped := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("dev: request dropped by a fault rule")}
	cases := []struct {
		name    string
		err     error
		noLead  bool
		unknown bool
	}{
		{"no leader", rpc.ErrNoLeader, true, false},
		{"not leader", rpc.ErrNotLeader, true, false},
		{"dial refused", dial, true, false},
		{"dial refused, wrapped", errors.Join(errors.New("rpc: node unavailable"), dial), true, false},
		{"dropped by a fault rule", dropped, true, false},
		{"read after send", &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, false, true},
		{"timeout", errors.New("context deadline exceeded"), false, true},
		{"leader's 500", errors.New("rpc: 500 internal"), false, true},
	}
	for _, c := range cases {
		got := forwardErr(c.err)
		if errors.Is(got, ErrNoLeader) != c.noLead || errors.Is(got, meta.ErrUnknownOutcome) != c.unknown {
			t.Errorf("%s: %v (no leader %v, unknown %v)", c.name, got, errors.Is(got, ErrNoLeader), errors.Is(got, meta.ErrUnknownOutcome))
		}
	}
}
