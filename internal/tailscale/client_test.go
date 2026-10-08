package tailscale

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var ctx = context.Background()

// stubCLI writes a shell script that answers `status --json` and `whois --json <ip>`
// from the fixtures, failing whois for unknown addresses like the real CLI does.
func stubCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub for the CLI; the parser is covered by the fixture tests")
	}
	dir := t.TempDir()
	status, _ := filepath.Abs("testdata/status.json")
	whois, _ := filepath.Abs("testdata/whois.json")
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  'status --json') cat '" + status + "' ;;\n" +
		"  'whois --json') if [ \"$3\" = 100.64.0.3 ]; then cat '" + whois + "'; else echo 'no match for IP' >&2; exit 1; fi ;;\n" +
		"  'slow x') sleep 5 ;;\n" +
		"  *) echo \"unknown: $*\" >&2; exit 2 ;;\n" +
		"esac\n"
	p := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCLI_StatusParsesHeadscaleOutput(t *testing.T) {
	c := &CLI{Path: stubCLI(t)}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.BackendState != "Running" || st.Self.ID != "27" || st.Self.HostName != "jockey" || st.MagicDNSSuffix != "8ball.local" {
		t.Fatalf("status %+v", st.Self)
	}
	ip, err := SelfIPv4(st)
	if err != nil || ip != netip.MustParseAddr("100.64.0.11") {
		t.Fatalf("self ip %v %v", ip, err)
	}
	if len(st.Peer) != 11 {
		t.Fatalf("%d peers", len(st.Peer))
	}
	var skel PeerStatus
	for _, p := range st.Peer {
		if p.HostName == "skel" {
			skel = p
		}
	}
	if skel.ID != "19" || !skel.Online || skel.OS != "linux" {
		t.Fatalf("skel %+v", skel)
	}
	if v4, ok := skel.IPv4(); !ok || v4 != netip.MustParseAddr("100.64.0.3") {
		t.Fatalf("skel ipv4 %v", v4)
	}
}

func TestCLI_WhoIs(t *testing.T) {
	c := &CLI{Path: stubCLI(t)}
	w, err := c.WhoIs(ctx, netip.MustParseAddr("100.64.0.3"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Node.StableID != "19" || w.Node.Name != "skel.8ball.local." || w.Node.Hostinfo.Hostname != "skel" || len(w.Node.Addresses) != 2 {
		t.Fatalf("whois %+v", w.Node)
	}
	if _, err := c.WhoIs(ctx, netip.MustParseAddr("100.64.0.250")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ip: %v", err)
	}
}

func TestCLI_TimeoutAndMissing(t *testing.T) {
	c := &CLI{Path: stubCLI(t), Timeout: 200 * time.Millisecond}
	start := time.Now()
	if _, err := c.run(ctx, "slow", "x"); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
	}
	if _, err := Find(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, ErrCLIMissing) {
		t.Fatalf("missing override: %v", err)
	}
	if p, err := Find(c.Path); err != nil || p != c.Path {
		t.Fatalf("override: %v %v", p, err)
	}
}

func TestSelfIPv4_NotRunning(t *testing.T) {
	st := &Status{BackendState: "NeedsLogin"}
	if _, err := SelfIPv4(st); !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
	st = &Status{BackendState: "Running", TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a::1")}}
	if _, err := SelfIPv4(st); !errors.Is(err, ErrNoSelfIPv4) {
		t.Fatal(err)
	}
}

func TestFake_RoundTripAndLoad(t *testing.T) {
	self := netip.MustParseAddr("100.64.0.1")
	peer := netip.MustParseAddr("100.64.0.2")
	f := NewFake(self, "1", map[netip.Addr]string{peer: "2"})
	st, _ := f.Status(ctx)
	if ip, _ := SelfIPv4(st); ip != self || len(st.Peer) != 1 {
		t.Fatalf("fake status %+v", st)
	}
	if w, err := f.WhoIs(ctx, peer); err != nil || w.Node.StableID != "2" {
		t.Fatalf("fake whois %+v %v", w, err)
	}
	f.Remove(peer)
	if _, err := f.WhoIs(ctx, peer); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed peer still known")
	}
	f.Err = errors.New("down")
	if _, err := f.Status(ctx); err == nil {
		t.Fatal("Err not honoured")
	}
	p := filepath.Join(t.TempDir(), "fake.json")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(p, []byte(`{"status":{"BackendState":"Running","TailscaleIPs":["100.64.0.9"],"Self":{"ID":"9","TailscaleIPs":["100.64.0.9"]}},"whois":{"100.64.0.9":{"Node":{"StableID":"9","Name":"n9."}}}}`), 0o600))
	lf, err := LoadFake(p)
	must(err)
	if w, err := lf.WhoIs(ctx, netip.MustParseAddr("100.64.0.9")); err != nil || w.Node.StableID != "9" {
		t.Fatalf("loaded fake %+v %v", w, err)
	}
	if _, err := LoadFake(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file")
	}
}
