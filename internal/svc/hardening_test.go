package svc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Linux unit confines the service to its data directory, its disks and its config
// directory, quotes every path, gives it 45 s to stop and is staged in the private
// directory the installer made; the LaunchAgent restarts only a failed exit.
//
// Invariant: I-122
func TestSvc_SystemdHardeningAndQuoting(t *testing.T) {
	p := Params{
		GOOS: "linux", Exe: "/opt/tail raid/tailraid", ConfigPath: "/home/u/.config/tailraid/config.yaml", Home: "/home/u/.config/tailraid",
		User: "u", DataDir: "/srv/tail raid/data", Disks: []string{"/mnt/a", "/mnt/b"}, Stage: "/run/user/1000/stage",
	}
	pl, err := Install(p)
	if err != nil {
		t.Fatal(err)
	}
	unit := pl.Files[0].Content
	for _, want := range []string{
		`ExecStart="/opt/tail raid/tailraid" serve --config /home/u/.config/tailraid/config.yaml`,
		"NoNewPrivileges=yes", "ProtectSystem=strict", "ProtectHome=read-only", "PrivateTmp=yes", "TimeoutStopSec=45",
		`ReadWritePaths="/srv/tail raid/data" /home/u/.config/tailraid /mnt/a /mnt/b`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if got := filepath.ToSlash(pl.Files[0].Path); got != "/run/user/1000/stage/tailraid.service" {
		t.Errorf("staged at %s", got)
	}
	p.Stage = ""
	pl, _ = Install(p)
	if !strings.HasPrefix(pl.Files[0].Path, os.TempDir()) {
		t.Errorf("without a stage the unit goes under the temp dir, got %s", pl.Files[0].Path)
	}
	p.GOOS = "darwin"
	pl, _ = Install(p)
	if !strings.Contains(pl.Files[0].Content, "<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>") {
		t.Error("the LaunchAgent must restart only a failed exit, so `tailraid stop` sticks")
	}
	for _, c := range []struct{ in, want string }{
		{"/plain/path", "/plain/path"},
		{"/a b", `"/a b"`},
		{`/q"x`, `"/q\"x"`},
		{`/back\slash`, `"/back\\slash"`},
	} {
		if got := unitQuote(c.in); got != c.want {
			t.Errorf("unitQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
