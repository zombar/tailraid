package svc

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The Linux unit grants CAP_NET_ADMIN and nothing else and waits for the network alone;
// the macOS plist is installed by root into the system domain and its log is the
// daemon's; the Windows service opens the mesh port's inbound firewall rule and closes
// it at uninstall, and a rule that was never there does not fail the uninstall.
// Invariant: I-324
func TestSvc_MeshPrivileges(t *testing.T) {
	p := Params{Exe: "/opt/tailraid", ConfigPath: "/etc/tailraid/config.yaml", Home: "/etc/tailraid", User: "u", UID: 501, MeshPort: 20483, Stage: "/stage"}
	p.GOOS = "linux"
	pl, err := Install(p)
	if err != nil {
		t.Fatal(err)
	}
	unit := pl.Files[0].Content
	for _, want := range []string{"AmbientCapabilities=CAP_NET_ADMIN\n", "CapabilityBoundingSet=CAP_NET_ADMIN\n", "After=network-online.target\n", "NoNewPrivileges=yes\n", "User=u\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "tailscaled") || strings.Count(unit, "CAP_") != 2 {
		t.Errorf("unit names more than the one capability, or tailscaled:\n%s", unit)
	}

	p.GOOS = "darwin"
	p.LogFile = "/Library/Logs/tailraid/serve.log"
	pl, err = Install(p)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(p.Stage, "app.tailraid.serve.plist") // the host's separators: the plan is built on any OS
	if pl.Files[0].Path != staged {
		t.Errorf("plist staged at %s, want %s", pl.Files[0].Path, staged)
	}
	cmds := make([]string, 0, len(pl.Commands))
	for _, c := range pl.Commands {
		cmds = append(cmds, strings.Join(c, " "))
	}
	for _, want := range []string{
		"sudo launchctl bootout system " + LaunchDaemonPath,
		"sudo install -m 644 -o root -g wheel " + staged + " " + LaunchDaemonPath,
		"sudo launchctl bootstrap system " + LaunchDaemonPath,
		"sudo launchctl kickstart -k system/app.tailraid.serve",
	} {
		if !containsString(cmds, want) {
			t.Errorf("darwin install lacks %q: %v", want, cmds)
		}
	}
	if !strings.Contains(pl.Files[0].Content, "<string>/Library/Logs/tailraid/serve.log</string>") {
		t.Errorf("the daemon's log: %s", pl.Files[0].Content)
	}
	un, _ := Uninstall(p)
	if got := strings.Join(un.Commands[0], " "); got != "sudo launchctl bootout system "+LaunchDaemonPath {
		t.Errorf("darwin uninstall: %s", got)
	}
	st, _ := Start(p)
	if got := strings.Join(st.Commands[0], " "); got != "sudo launchctl kickstart system/app.tailraid.serve" {
		t.Errorf("darwin start: %s", got)
	}
	p.Root = true
	st, _ = Start(p)
	if st.Commands[0][0] == "sudo" {
		t.Error("root must not sudo")
	}

	p.GOOS, p.Root = "windows", false
	p.Exe, p.ConfigPath = `C:\tailraid\tailraid.exe`, `C:\ProgramData\tailraid\config.yaml`
	pl, err = Install(p)
	if err != nil {
		t.Fatal(err)
	}
	cmds = cmds[:0]
	for _, c := range pl.Commands {
		cmds = append(cmds, strings.Join(c, " "))
	}
	if !containsString(cmds, "netsh advfirewall firewall add rule name=tailraid mesh dir=in action=allow protocol=UDP localport=20483") {
		t.Errorf("windows install opens no firewall rule: %v", cmds)
	}
	if !containsString(pl.Notes, "wintun.dll beside "+p.Exe+" (docs/20 §9)") {
		t.Errorf("notes %v", pl.Notes)
	}
	un, _ = Uninstall(p)
	del := []string{"netsh", "advfirewall", "firewall", "delete", "rule", "name=tailraid mesh"}
	found := false
	for _, c := range un.Commands {
		if strings.Join(c, " ") == strings.Join(del, " ") {
			found = true
		}
	}
	if !found {
		t.Errorf("windows uninstall keeps the firewall rule: %v", un.Commands)
	}
	if !notRunning(del, "No rules match the specified criteria.") {
		t.Error("a rule that is not there must not fail the uninstall")
	}
}

func containsString(list []string, want string) bool { return slices.Contains(list, want) }
