package admin_test

import (
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/vfs"
)

// The gateway view sums dirty and plan bytes as staged; the line shows them with the
// time to drain at the measured rate, and nothing when nothing is staged.
// Invariant: I-398
func TestGatewayReport_Staged(t *testing.T) {
	g := admin.GatewayFromDirty([]vfs.DirtyInfo{{DirtyBytes: 3 << 30, PlanBytes: 1 << 30}, {DirtyBytes: 1 << 20}})
	if want := uint64(4<<30 + 1<<20); g.Staged != want {
		t.Fatalf("staged %d, want %d", g.Staged, want)
	}
	cases := []struct {
		staged, rate uint64
		want         string
	}{
		{0, 0, ""},
		{1 << 20, 0, "1.0 MiB on this host only"},
		{60 << 20, 1 << 20, "60.0 MiB on this host only, draining 1.0 MiB/s, about 1 min"},
		{30 << 20, 1 << 20, "30.0 MiB on this host only, draining 1.0 MiB/s, about 30 s"},
		{41 << 30, 38 << 20, "41.0 GiB on this host only, draining 38.0 MiB/s, about 18 min"},
		{400 << 30, 38 << 20, "400.0 GiB on this host only, draining 38.0 MiB/s, about 3 h 0 min"},
	}
	for _, c := range cases {
		got := admin.StagedLine(&admin.GatewayReport{Staged: c.staged, DrainRate: c.rate})
		if got != c.want {
			t.Errorf("staged %d at %d/s: %q, want %q", c.staged, c.rate, got, c.want)
		}
	}
	if admin.StagedLine(nil) != "" {
		t.Fatal("a nil gateway has a line")
	}
}

// status and check warn while more than 1 GiB is staged: {0, 1, max−1, max, max+1}.
// Invariant: I-398
func TestCheck_WarnsStaged(t *testing.T) {
	cases := []struct {
		staged uint64
		warn   bool
	}{
		{0, false},
		{1, false},
		{admin.StageWarnAt - 1, false},
		{admin.StageWarnAt, false},
		{admin.StageWarnAt + 1, true},
	}
	for _, c := range cases {
		w := admin.StagedWarning(&admin.GatewayReport{Staged: c.staged})
		if (w != "") != c.warn {
			t.Errorf("staged %d: warning %q, want one %v", c.staged, w, c.warn)
		}
		if c.warn && !strings.Contains(w, "on this host only") {
			t.Errorf("warning %q does not say where the bytes are", w)
		}
	}
}
