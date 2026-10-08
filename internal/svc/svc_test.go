package svc

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every unit routes the daemon's output somewhere a human can read it (I-60): macOS to the
// log file via launchd, Linux to journald, Windows via log.file because services have no
// stderr; each unit restarts the process and runs the same binary with the same config.
// Invariant: I-60
func TestSvc_UnitsRouteLogs(t *testing.T) {
	p := Params{Exe: "/opt/tailraid", ConfigPath: "/home/u/.config/tailraid/config.yaml", Home: "/Users/u/Library/Application Support/tailraid",
		LogFile: "/var/log/tailraid.log", User: "u", UID: 501}
	p.GOOS = "darwin"
	pl, err := Install(p)
	if err != nil || len(pl.Files) != 1 {
		t.Fatalf("darwin: %v %+v", err, pl)
	}
	plist := pl.Files[0].Content
	for _, want := range []string{"<string>/opt/tailraid</string><string>serve</string>", "<key>KeepAlive</key><true/>", "<key>StandardErrorPath</key><string>/var/log/tailraid.log</string>", "<key>StandardOutPath</key><string>/var/log/tailraid.log</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if !strings.HasSuffix(filepath.ToSlash(pl.Files[0].Path), "/Users/u/Library/LaunchAgents/app.tailraid.serve.plist") {
		t.Errorf("plist path %s", pl.Files[0].Path)
	}
	if got := strings.Join(pl.Commands[1], " "); got != "launchctl bootstrap gui/501 "+pl.Files[0].Path {
		t.Errorf("bootstrap: %s", got)
	}

	p.GOOS = "linux"
	pl, err = Install(p)
	if err != nil {
		t.Fatal(err)
	}
	unit := pl.Files[0].Content
	for _, want := range []string{"ExecStart=/opt/tailraid serve --config /home/u/.config/tailraid/config.yaml", "User=u", "Restart=always", "After=network-online.target tailscaled.service", "Environment=TAILRAID_HOME=/Users/u/Library/Application Support/tailraid"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	if strings.Contains(unit, "StandardOutput") {
		t.Error("linux unit must leave stdout/stderr to journald")
	}
	if got := strings.Join(pl.Commands[2], " "); got != "sudo systemctl enable --now tailraid" {
		t.Errorf("enable: %s", got)
	}
	if got := strings.Join(Logs(p, true, 50), " "); got != "journalctl -u tailraid -n 50 --no-pager -f" {
		t.Errorf("logs: %s", got)
	}
	p.Root = true
	pl, _ = Install(p)
	if pl.Commands[1][0] == "sudo" {
		t.Error("root must not sudo")
	}

	p.GOOS = "windows"
	p.Exe, p.ConfigPath = `C:\tailraid\tailraid.exe`, `C:\ProgramData\tailraid\config.yaml`
	pl, err = Install(p)
	if err != nil || len(pl.Files) != 0 {
		t.Fatalf("windows: %v %+v", err, pl)
	}
	create := strings.Join(pl.Commands[0], " ")
	for _, want := range []string{`binPath= "C:\tailraid\tailraid.exe" serve --config "C:\ProgramData\tailraid\config.yaml" --service`, "start= delayed-auto", "depend= Tailscale"} {
		if !strings.Contains(create, want) {
			t.Errorf("sc create lacks %q: %s", want, create)
		}
	}
	if !strings.Contains(strings.Join(pl.Commands[2], " "), "actions= restart/5000") {
		t.Errorf("no failure actions: %v", pl.Commands[2])
	}
	if Logs(p, false, 10) != nil {
		t.Error("windows logs are a file tailraid tails itself")
	}
	if _, err := Uninstall(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Params{GOOS: "plan9"}); err == nil {
		t.Fatal("unsupported OS accepted")
	}
}
