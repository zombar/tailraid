package admin_test

import (
	"os"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/testharness"
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

// The gateway view carries what the gateway proposed by op and the SETATTRs it held for a
// file's flush (docs/12 §8).
// Invariant: I-403
func TestGatewayReport_Proposals(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	p := vfs.CountProposals(f)
	fs := vfs.New(f.FSM, p, f.Ov, f.Reader(), f.Clk, 501, 20)
	fh, err := fs.OpenFile("/a", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	if err := fs.Chmod("/a", 0o600); err != nil { // held: the file is dirty here
		t.Fatal(err)
	}
	if err := fs.MkdirAll("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	g := admin.GatewayFromDirty(nil).WithFS(fs).WithProposals(p)
	if g.Proposals["create"] != 1 || g.Proposals["mkdir"] != 1 || g.Proposals["setattr"] != 0 || g.AttrsDeferred != 1 {
		t.Fatalf("proposals %v, attrs deferred %d", g.Proposals, g.AttrsDeferred)
	}
}
