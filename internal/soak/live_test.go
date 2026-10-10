package soak

import (
	"strings"
	"testing"
)

// The live environment's drop rules: a held-down node drops every peer and every peer
// drops it; a partition cuts the two sides; the two compose, and a heal keeps the holds.
// The device running verify is never dropped by anyone (the driver must reach every node
// to lift the rules), while its own node drops whatever the table says.
func TestLiveEnv_HoldDownRules(t *testing.T) {
	all := []NodeRef{{Name: "a", Idx: 1, RPC: "10.0.0.1:1"}, {Name: "b", Idx: 2, RPC: "10.0.0.2:1"}, {Name: "c", Idx: 3, RPC: "10.0.0.3:1"}, {Name: "d", Idx: 4, RPC: "10.0.0.4:1"}}
	rules := func(t *faultTable) string {
		var out []string
		for _, n := range all {
			out = append(out, n.Name+":"+strings.Join(t.rules(n, all, ""), ","))
		}
		return strings.Join(out, " ")
	}
	t.Run("verifier kept reachable", func(t *testing.T) {
		var ft faultTable
		ft.hold(all[3], true)
		ft.partition(all[:1], all[1:3])
		var out []string
		for _, n := range all {
			out = append(out, n.Name+":"+strings.Join(ft.rules(n, all, "10.0.0.1:1"), ","))
		}
		if got := strings.Join(out, " "); got != "a:10.0.0.2,10.0.0.3,10.0.0.4 b:10.0.0.4 c:10.0.0.4 d:10.0.0.2,10.0.0.3" {
			t.Fatalf("held d + partition a|bc, verify on a: %q", got)
		}
	})
	var ft faultTable
	if got := rules(&ft); got != "a: b: c: d:" {
		t.Fatalf("empty: %q", got)
	}
	ft.hold(all[3], true)
	if got := rules(&ft); got != "a:10.0.0.4 b:10.0.0.4 c:10.0.0.4 d:10.0.0.1,10.0.0.2,10.0.0.3" {
		t.Fatalf("held d: %q", got)
	}
	ft.partition(all[:1], all[1:3])
	if got := rules(&ft); got != "a:10.0.0.2,10.0.0.3,10.0.0.4 b:10.0.0.1,10.0.0.4 c:10.0.0.1,10.0.0.4 d:10.0.0.1,10.0.0.2,10.0.0.3" {
		t.Fatalf("held d + partition a|bc: %q", got)
	}
	ft.partition(nil, nil) // heal keeps the hold
	if got := rules(&ft); got != "a:10.0.0.4 b:10.0.0.4 c:10.0.0.4 d:10.0.0.1,10.0.0.2,10.0.0.3" {
		t.Fatalf("healed: %q", got)
	}
	ft.hold(all[3], false)
	if got := rules(&ft); got != "a: b: c: d:" {
		t.Fatalf("released: %q", got)
	}
}

// A live run's capabilities follow its flags: nothing destructive without --destructive,
// and no kill, restart or partition without --restarts as well.
//
// Invariant: I-113
func TestLiveEnv_CapsFollowFlags(t *testing.T) {
	for _, c := range []struct {
		destructive, restarts bool
		want                  Caps
	}{
		{false, false, Caps{}},
		{true, false, Caps{Corrupt: true, Mutate: true}},
		{true, true, Caps{Corrupt: true, Mutate: true, Kill: true, Partition: true}},
		{false, true, Caps{}}, // verify refuses the combination; the env is safe anyway
	} {
		e := &LiveEnv{Destructive: c.destructive, Restarts: c.restarts}
		if got := e.Caps(); got != c.want {
			t.Errorf("destructive=%v restarts=%v: caps %+v, want %+v", c.destructive, c.restarts, got, c.want)
		}
	}
}
