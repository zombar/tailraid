package admin

import (
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/mesh/firewall"
)

// An own mesh address the node cannot reach is a warning carrying the error and the
// cause found; a chain that could not be kept is one while the address is reachable;
// a healthy or absent mesh warns nothing.
// Invariant: I-334, I-335
func TestMeshReport_SelfReachWarns(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    *MeshReport
		want []string // substrings of the one warning; nil: none
	}{
		{"absent", nil, nil},
		{"unchecked", &MeshReport{Addr: "100.79.112.1"}, nil},
		{"reachable", &MeshReport{Addr: "100.79.112.1", SelfReach: &SelfReach{OK: true}, Firewall: &firewall.Status{Backend: "iptables", Kept: true}}, nil},
		{
			"unreachable", &MeshReport{Addr: "100.79.112.1", SelfReach: &SelfReach{Err: "i/o timeout", Cause: "tailscaled's ts-input drops 100.64/10"}},
			[]string{"100.79.112.1", "i/o timeout", "ts-input"},
		},
		{
			"chain failed", &MeshReport{Addr: "100.79.112.1", SelfReach: &SelfReach{OK: true}, Firewall: &firewall.Status{Backend: "iptables", Err: "permission denied"}},
			[]string{"firewall chain", "permission denied"},
		},
	} {
		got := MeshWarnings(tc.m)
		if tc.want == nil {
			if len(got) != 0 {
				t.Errorf("%s: warnings %q", tc.name, got)
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("%s: %d warnings %q, want one", tc.name, len(got), got)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(got[0], w) {
				t.Errorf("%s: %q lacks %q", tc.name, got[0], w)
			}
		}
	}
}
