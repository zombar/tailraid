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
	p := Params{
		Exe: "/opt/tailraid", ConfigPath: "/home/u/.config/tailraid/config.yaml", Home: "/Users/u/Library/Application Support/tailraid",
		LogFile: "/var/log/tailraid.log", User: "u", UID: 501,
	}
	p.GOOS = "darwin"
	pl, err := Install(p)
	if err != nil || len(pl.Files) != 1 {
		t.Fatalf("darwin: %v %+v", err, pl)
	}
	plist := pl.Files[0].Content
	for _, want := range []string{"<string>/opt/tailraid</string><string>serve</string>", "<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>", "<key>StandardErrorPath</key><string>/var/log/tailraid.log</string>", "<key>StandardOutPath</key><string>/var/log/tailraid.log</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if !strings.HasSuffix(filepath.ToSlash(pl.Files[0].Path), "/app.tailraid.serve.plist") || strings.Contains(pl.Files[0].Path, "LaunchDaemons") {
		t.Errorf("the plist is staged, not written to the daemons directory by the user: %s", pl.Files[0].Path)
	}
	if got := strings.Join(pl.Commands[2], " "); got != "sudo launchctl bootstrap system "+LaunchDaemonPath {
		t.Errorf("bootstrap: %s", got)
	}
	if got := strings.Join(pl.Commands[1], " "); !strings.HasPrefix(got, "sudo install -m 644 -o root -g wheel ") || !strings.HasSuffix(got, LaunchDaemonPath) {
		t.Errorf("install: %s", got)
	}

	p.GOOS = "linux"
	pl, err = Install(p)
	if err != nil {
		t.Fatal(err)
	}
	unit := pl.Files[0].Content
	for _, want := range []string{"ExecStart=/opt/tailraid serve --config /home/u/.config/tailraid/config.yaml", "User=u", "Restart=always", "After=network-online.target\n", `Environment="TAILRAID_HOME=/Users/u/Library/Application Support/tailraid"`} {
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
	for _, want := range []string{`binPath= "C:\tailraid\tailraid.exe" serve --config "C:\ProgramData\tailraid\config.yaml" --service`, "start= delayed-auto"} {
		if !strings.Contains(create, want) {
			t.Errorf("sc create lacks %q: %s", want, create)
		}
	}
	if strings.Contains(create, "depend=") {
		t.Errorf("the service depends on nothing since the mesh: %s", create)
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

// On Windows the install plan maps the drive at the installing user's logon, in that
// user's desktop session with the limited token (Explorer's; an elevated session's drive
// is invisible to Explorer), with no console window, and starts the task once; uninstall
// removes it whether or not it exists. A quote in a path is doubled for PowerShell.
// Invariant: I-142
func TestSvc_Windows_LogonMount(t *testing.T) {
	p := Params{GOOS: "windows", Exe: `C:\Program Files\tail'raid\tailraid.exe`, ConfigPath: `C:\ProgramData\tailraid\config.yaml`, User: `HONKER\o'neil`, LogonMount: true}
	pl, err := Install(p)
	if err != nil {
		t.Fatal(err)
	}
	find := func(cmds [][]string, word string) int {
		for i, c := range cmds {
			if strings.Contains(strings.Join(c, " "), word) {
				return i
			}
		}
		return -1
	}
	reg, start, scStart := find(pl.Commands, "Register-ScheduledTask"), find(pl.Commands, "Start-ScheduledTask"), find(pl.Commands, "sc.exe start")
	if reg < 0 || start < 0 || start < reg || start < scStart {
		t.Fatalf("register %d, start %d, sc start %d: %v", reg, start, scStart, pl.Commands)
	}
	if c := pl.Commands[reg]; c[0] != "powershell" || c[len(c)-2] != "-Command" {
		t.Fatalf("register runs %v", c)
	}
	script := pl.Commands[reg][len(pl.Commands[reg])-1]
	for _, want := range []string{
		"-TaskName 'tailraid-mount'",
		"New-ScheduledTaskTrigger -AtLogOn -User 'HONKER\\o''neil'",
		"New-ScheduledTaskPrincipal -UserId 'HONKER\\o''neil' -LogonType Interactive -RunLevel Limited",
		"New-ScheduledTaskAction -Execute 'conhost.exe' -Argument '--headless \"C:\\Program Files\\tail''raid\\tailraid.exe\" mount --wait 5m --config \"C:\\ProgramData\\tailraid\\config.yaml\"'",
		"-Force",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("register lacks %q:\n%s", want, script)
		}
	}
	if got := pl.Commands[start][len(pl.Commands[start])-1]; got != "Start-ScheduledTask -TaskName 'tailraid-mount'" {
		t.Errorf("start: %q", got)
	}
	un, err := Uninstall(p)
	if err != nil {
		t.Fatal(err)
	}
	last := strings.Join(un.Commands[len(un.Commands)-1], " ")
	if !strings.Contains(last, "Unregister-ScheduledTask -TaskName 'tailraid-mount' -Confirm:$false -ErrorAction SilentlyContinue") {
		t.Errorf("uninstall ends with %q", last)
	}
	// no gateway or no drive configured: no task
	p.LogonMount = false
	pl, _ = Install(p)
	un, _ = Uninstall(p)
	if find(pl.Commands, "ScheduledTask") >= 0 || find(un.Commands, "ScheduledTask") >= 0 {
		t.Errorf("a task without LogonMount: %v %v", pl.Commands, un.Commands)
	}
}
