// Package svc plans and applies the per-OS service installation of `tailraid serve`
// (docs/12 §1): a LaunchAgent on macOS, a systemd system unit on Linux, an SCM service on
// Windows. Plans are data so `install --print` can show them and tests can check them.
package svc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Name is the service name on every OS.
const Name = "tailraid"

// Params describe the installation target.
type Params struct {
	GOOS       string
	Exe        string   // absolute path of the tailraid binary
	ConfigPath string   // the config file serve will read
	Home       string   // config.Home() of the installing user
	LogFile    string   // where serve's output goes (macOS, Windows)
	User       string   // the installing user (Linux unit User=)
	UID        int      // the installing user's uid (macOS launchctl domain)
	Root       bool     // running as root/elevated already (no sudo prefix)
	DataDir    string   // the node's data directory (Linux: the unit may write there, I-122)
	Disks      []string // added disk paths (Linux: likewise)
	Stage      string   // Linux: a private directory the unit file is staged in before `install` copies it; "" uses os.TempDir()
	LogonMount bool     // Windows: map mount.path at the user's logon (listen.smb and mount.path set; I-142)
}

// MountTask is the Windows scheduled task that maps the drive at logon (I-142).
const MountTask = "tailraid-mount"

// File is a unit file to write.
type File struct {
	Path    string
	Mode    os.FileMode
	Content string
}

// Plan is what install/uninstall will do, in order.
type Plan struct {
	Files    []File
	Commands [][]string
	// WaitStopped is the manager query Apply repeats after the commands until it reports
	// no running process, bounded by stopTimeout: `launchctl kill` and `sc.exe stop`
	// return once the signal is delivered, while the process is still handing over
	// leadership and flushing, and a start issued before it exits is lost (I-131).
	WaitStopped []string
	Notes       []string
}

const (
	stopTimeout = 60 * time.Second       // a leader hands over (5 s) and flushes (30 s) before it exits (I-123)
	stopPoll    = 500 * time.Millisecond // between WaitStopped queries
)

// Install returns the plan that registers and starts the service.
func Install(p Params) (Plan, error) {
	switch p.GOOS {
	case "darwin":
		plist := launchAgentPath(p)
		return Plan{
			Files: []File{{Path: plist, Mode: 0o644, Content: launchdPlist(p)}},
			Commands: [][]string{
				{"launchctl", "bootout", "gui/" + strconv.Itoa(p.UID), plist}, // ignore failure: not loaded yet
				{"launchctl", "bootstrap", "gui/" + strconv.Itoa(p.UID), plist},
				{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/app.tailraid.serve", p.UID)},
			},
			Notes: []string{"logs: " + p.LogFile, "mount after login with: tailraid mount"},
		}, nil
	case "linux":
		unit := unitPath
		stage := p.Stage
		if stage == "" {
			stage = os.TempDir()
		}
		tmp := path.Join(stage, "tailraid.service") // a Linux plan: slash paths whatever OS builds it
		sudo := sudoPrefix(p)
		return Plan{
			Files: []File{{Path: tmp, Mode: 0o644, Content: systemdUnit(p)}},
			Commands: [][]string{
				append(sudo, "install", "-m", "644", tmp, unit),
				append(sudo, "systemctl", "daemon-reload"),
				append(sudo, "systemctl", "enable", "--now", Name),
			},
			Notes: []string{"logs: journalctl -u tailraid (tailraid logs)", "mount after login with: tailraid mount"},
		}, nil
	case "windows":
		bin := `"` + p.Exe + `" serve --config "` + p.ConfigPath + `" --service`
		pl := Plan{
			Commands: [][]string{
				{"sc.exe", "create", Name, "binPath=", bin, "start=", "delayed-auto", "depend=", "Tailscale", "DisplayName=", "tailraid"},
				{"sc.exe", "description", Name, "tailraid node: metadata, shard store and gateways"},
				{"sc.exe", "failure", Name, "reset=", "60", "actions=", "restart/5000/restart/5000/restart/5000"},
				{"sc.exe", "start", Name},
			},
			Notes: []string{"log.file set to " + p.LogFile + " in " + p.ConfigPath + " (services have no stderr)", "run from an elevated prompt"},
		}
		if p.LogonMount {
			pl.Commands = append(pl.Commands, powershell(registerMountTask(p)), powershell("Start-ScheduledTask -TaskName "+psQuote(MountTask)))
			pl.Notes = append(pl.Notes, "the drive is mapped at every logon of "+p.User+" by the task "+MountTask)
		} else {
			pl.Notes = append(pl.Notes, "mount after login with: tailraid mount T:")
		}
		return pl, nil
	}
	return Plan{}, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
}

// unitPath is the systemd unit `install` writes.
const unitPath = "/etc/systemd/system/tailraid.service"

// UnitInstalled reports whether the systemd unit is installed on this machine.
func UnitInstalled() bool {
	_, err := os.Stat(unitPath)
	return err == nil
}

// RefreshUnit returns the plan that rewrites the systemd unit from p and restarts the
// service, so a disk added since `install` is among the paths the sandbox lets serve
// write (I-122, I-147). Off Linux nothing confines the paths: the plan is empty.
func RefreshUnit(p Params) (Plan, error) {
	if p.GOOS != "linux" {
		return Plan{}, nil
	}
	stage := p.Stage
	if stage == "" {
		stage = os.TempDir()
	}
	tmp := path.Join(stage, "tailraid.service") // a Linux plan: slash paths whatever OS builds it
	sudo := sudoPrefix(p)
	return Plan{
		Files: []File{{Path: tmp, Mode: 0o644, Content: systemdUnit(p)}},
		Commands: [][]string{
			append(slices.Clone(sudo), "install", "-m", "644", tmp, unitPath),
			append(slices.Clone(sudo), "systemctl", "daemon-reload"),
			append(slices.Clone(sudo), "systemctl", "restart", Name),
		},
	}, nil
}

// Uninstall returns the plan that stops and removes the service (data stays).
func Uninstall(p Params) (Plan, error) {
	switch p.GOOS {
	case "darwin":
		plist := launchAgentPath(p)
		return Plan{Commands: [][]string{{"launchctl", "bootout", "gui/" + strconv.Itoa(p.UID), plist}, {"rm", "-f", plist}}}, nil
	case "linux":
		sudo := sudoPrefix(p)
		return Plan{Commands: [][]string{
			append(sudo, "systemctl", "disable", "--now", Name),
			append(sudo, "rm", "-f", unitPath),
			append(sudo, "systemctl", "daemon-reload"),
		}}, nil
	case "windows":
		pl := Plan{Commands: [][]string{{"sc.exe", "stop", Name}, {"sc.exe", "delete", Name}}}
		if p.LogonMount {
			pl.Commands = append(pl.Commands, powershell("Unregister-ScheduledTask -TaskName "+psQuote(MountTask)+" -Confirm:$false -ErrorAction SilentlyContinue"))
		}
		return pl, nil
	}
	return Plan{}, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
}

// registerMountTask is the PowerShell that registers MountTask for the installing user:
// at that user's logon, in the interactive session with the limited token (Explorer's
// session: a drive an elevated session maps is invisible to Explorer), `tailraid mount
// --wait` under a headless console, so no window opens while it waits for the service.
func registerMountTask(p Params) string {
	arg := `--headless "` + p.Exe + `" mount --wait 5m --config "` + p.ConfigPath + `"`
	u := psQuote(p.User)
	return "Register-ScheduledTask -Force -TaskName " + psQuote(MountTask) +
		" -Action (New-ScheduledTaskAction -Execute 'conhost.exe' -Argument " + psQuote(arg) + ")" +
		" -Trigger (New-ScheduledTaskTrigger -AtLogOn -User " + u + ")" +
		" -Principal (New-ScheduledTaskPrincipal -UserId " + u + " -LogonType Interactive -RunLevel Limited)" +
		" -Settings (New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Minutes 10))" +
		" | Out-Null"
}

func powershell(script string) []string {
	return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", script}
}

// psQuote is s as a PowerShell single-quoted string: nothing inside is expanded and a
// quote is doubled.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Start returns the plan that starts the installed service.
func Start(p Params) (Plan, error) {
	switch p.GOOS {
	case "darwin":
		return Plan{Commands: [][]string{{"launchctl", "kickstart", launchdTarget(p)}}}, nil
	case "linux":
		return Plan{Commands: [][]string{append(sudoPrefix(p), "systemctl", "start", Name)}}, nil
	case "windows":
		return Plan{Commands: [][]string{{"sc.exe", "start", Name}}}, nil
	}
	return Plan{}, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
}

// Stop returns the plan that stops the service and returns once it has no process, so a
// start that follows it starts the new binary; a service that was not running is already
// stopped (I-131). systemctl stop blocks on its own; launchctl and sc.exe are polled.
func Stop(p Params) (Plan, error) {
	switch p.GOOS {
	case "darwin":
		return Plan{
			Commands:    [][]string{{"launchctl", "kill", "SIGTERM", launchdTarget(p)}},
			WaitStopped: []string{"launchctl", "print", launchdTarget(p)},
		}, nil
	case "linux":
		return Plan{Commands: [][]string{append(sudoPrefix(p), "systemctl", "stop", Name)}}, nil
	case "windows":
		return Plan{
			Commands:    [][]string{{"sc.exe", "stop", Name}},
			WaitStopped: []string{"sc.exe", "query", Name},
		}, nil
	}
	return Plan{}, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
}

func launchdTarget(p Params) string { return fmt.Sprintf("gui/%d/app.tailraid.serve", p.UID) }

// scStopped matches the STATE line of `sc.exe query` for a stopped service.
var scStopped = regexp.MustCompile(`STATE\s*:\s*1\s+STOPPED`)

// stopped reads a WaitStopped query: a query that fails names a service that is not
// loaded or not installed, so nothing runs; otherwise the manager's state line decides.
func stopped(query []string, out string, err error) bool {
	if err != nil {
		return true
	}
	if query[0] == "launchctl" {
		return strings.Contains(out, "state = not running")
	}
	return scStopped.MatchString(out)
}

// notRunning reports a failed stop command whose output says there was no process to
// stop: launchctl's exit 3 and sc.exe's error 1062. Stopping a stopped service succeeds.
func notRunning(c []string, out string) bool {
	switch {
	case c[0] == "launchctl" && c[1] == "kill":
		return strings.Contains(out, "No process to signal")
	case c[0] == "sc.exe" && c[1] == "stop":
		return strings.Contains(out, "1062") || strings.Contains(out, "has not been started")
	}
	return false
}

// Logs returns the command that prints (or follows) the service log, or "" when the log
// is a plain file tailraid should tail itself.
func Logs(p Params, follow bool, lines int) []string {
	if p.GOOS == "linux" {
		cmd := []string{"journalctl", "-u", Name, "-n", strconv.Itoa(lines), "--no-pager"}
		if follow {
			cmd = append(cmd, "-f")
		}
		return cmd
	}
	return nil
}

// launchAgentPath is ~/Library/LaunchAgents/app.tailraid.serve.plist, derived from the
// config home (~/Library/Application Support/tailraid).
func launchAgentPath(p Params) string {
	lib := filepath.Dir(filepath.Dir(p.Home))
	if p.Home == "" {
		lib = filepath.Join(os.Getenv("HOME"), "Library")
	}
	return filepath.Join(lib, "LaunchAgents", "app.tailraid.serve.plist")
}

func sudoPrefix(p Params) []string {
	if p.Root {
		return nil
	}
	return []string{"sudo"}
}

func launchdPlist(p Params) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>app.tailraid.serve</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>serve</string><string>--config</string><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, p.Exe, p.ConfigPath, p.LogFile, p.LogFile)
}

// systemdUnit is the Linux unit (docs/12 §1, I-122): the process may write only its
// data directory, its disks and its config directory (ProtectSystem=strict with
// ReadWritePaths), never gains privileges, sees a private /tmp, and gets 45 s to stop
// (the final flush is capped at 30 s and a leader hands over first).
func systemdUnit(p Params) string {
	// the unit is Linux's whatever OS builds it (Params.GOOS), so its paths are slash paths
	rw := []string{p.DataDir, path.Dir(filepath.ToSlash(p.ConfigPath))}
	rw = append(rw, p.Disks...)
	if p.LogFile != "" {
		rw = append(rw, path.Dir(filepath.ToSlash(p.LogFile)))
	}
	var paths []string
	seen := map[string]bool{}
	for _, d := range rw {
		if d == "" || d == "." || seen[d] {
			continue
		}
		seen[d] = true
		paths = append(paths, unitQuote(d))
	}
	return fmt.Sprintf(`[Unit]
Description=tailraid node: metadata, shard store and NFS gateway
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
ExecStart=%s serve --config %s
User=%s
Environment=%s
Restart=always
RestartSec=2
TimeoutStopSec=45
LimitNOFILE=65536
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`, unitQuote(p.Exe), unitQuote(p.ConfigPath), p.User, unitQuote("TAILRAID_HOME="+p.Home), strings.Join(paths, " "))
}

// unitQuote quotes a path for a systemd unit when it holds a space or a quote.
func unitQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Print writes the plan in a readable form.
func Print(w io.Writer, pl Plan) {
	for _, f := range pl.Files {
		fmt.Fprintf(w, "# write %s (mode %o)\n%s\n", f.Path, f.Mode, f.Content)
	}
	for _, c := range pl.Commands {
		fmt.Fprintf(w, "+ %s\n", strings.Join(c, " "))
	}
	if pl.WaitStopped != nil {
		fmt.Fprintf(w, "# then repeat `%s` until the service has stopped (up to %s)\n", strings.Join(pl.WaitStopped, " "), stopTimeout)
	}
	for _, n := range pl.Notes {
		fmt.Fprintf(w, "# %s\n", n)
	}
}

// Apply writes the files and runs the commands, echoing each; a failing "bootout" or
// "stop" before install is tolerated (nothing was loaded yet), and so is a stop of a
// service with no process. With WaitStopped it then polls until the service has stopped.
func Apply(ctx context.Context, w io.Writer, pl Plan) error {
	for _, f := range pl.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(f.Path, []byte(f.Content), f.Mode); err != nil {
			return err
		}
		fmt.Fprintf(w, "wrote %s\n", f.Path)
	}
	for i, c := range pl.Commands {
		fmt.Fprintf(w, "+ %s\n", strings.Join(c, " "))
		var out strings.Builder
		cmd := exec.CommandContext(ctx, c[0], c[1:]...) //nolint:gosec // the plan's commands are launchctl, systemctl and sc.exe (docs/12 §1)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = io.MultiWriter(w, &out), io.MultiWriter(w, &out), os.Stdin
		if err := cmd.Run(); err != nil {
			tolerated := (i == 0 && (c[1] == "bootout" || c[1] == "stop")) || notRunning(c, out.String())
			if tolerated {
				continue
			}
			if _, ok := errors.AsType[*exec.ExitError](err); ok {
				return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
			}
			return err
		}
	}
	if pl.WaitStopped != nil {
		if err := waitStopped(ctx, pl.WaitStopped); err != nil {
			return err
		}
		fmt.Fprintln(w, "stopped")
	}
	for _, n := range pl.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	return nil
}

// waitStopped runs query until stopped reports no process, or stopTimeout passes.
func waitStopped(ctx context.Context, query []string) error {
	deadline := time.NewTimer(stopTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(stopPoll)
	defer tick.Stop()
	for {
		out, err := exec.CommandContext(ctx, query[0], query[1:]...).CombinedOutput() //nolint:gosec // launchctl print or sc.exe query (docs/12 §1)
		if stopped(query, string(out), err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("svc: the service is still running %s after stop", stopTimeout)
		case <-tick.C:
		}
	}
}
