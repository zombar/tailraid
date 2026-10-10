package node

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
)

// The gateway is stale in the Stale state, or without a leader for the threshold dated
// from the last leader contact, or, for a node that was the leader (Raft dates no
// contact for it), from when it first saw no leader; an election shorter than the
// threshold is never stale, and a leader's return resets the clock.
// Invariant: I-39
func TestStaleness_Table(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	th := cluster.StaleThreshold
	var none time.Time
	steps := []struct {
		name        string
		state       cluster.HealthState
		lastContact time.Time
		at          time.Duration
		want        bool
	}{
		{"healthy", cluster.Healthy, t0, 0, false},
		{"stale state", cluster.Stale, t0, 0, true},
		{"election, recent contact", cluster.NoLeader, t0, time.Second, false},
		{"election, contact older than the threshold", cluster.NoLeader, t0, th + time.Second, true},
		{"healthy again resets", cluster.Healthy, t0, th + 2*time.Second, false},
		{"former leader, no contact: just noticed", cluster.NoLeader, none, th + 3*time.Second, false},
		{"former leader, no contact: before the threshold", cluster.NoLeader, none, 2*th + 2*time.Second, false},
		{"former leader, no contact: threshold passed", cluster.NoLeader, none, 2*th + 3*time.Second, true},
		{"leader back", cluster.Healthy, none, 2*th + 4*time.Second, false},
		{"former leader again: the clock restarted", cluster.NoLeader, none, 2*th + 5*time.Second, false},
	}
	var s staleness
	for _, st := range steps {
		if got := s.stale(st.state, st.lastContact, t0.Add(st.at)); got != st.want {
			t.Fatalf("%s: stale %v, want %v", st.name, got, st.want)
		}
	}
}
