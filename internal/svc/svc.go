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
	"path/filepath"
	"strconv"
	"strings"
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
}

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
	Notes    []string
}

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
		unit := "/etc/systemd/system/tailraid.service"
		stage := p.Stage
		if stage == "" {
			stage = os.TempDir()
		}
		tmp := filepath.Join(stage, "tailraid.service")
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
		return Plan{
			Commands: [][]string{
				{"sc.exe", "create", Name, "binPath=", bin, "start=", "delayed-auto", "depend=", "Tailscale", "DisplayName=", "tailraid"},
				{"sc.exe", "description", Name, "tailraid node: metadata, shard store and gateways"},
				{"sc.exe", "failure", Name, "reset=", "60", "actions=", "restart/5000/restart/5000/restart/5000"},
				{"sc.exe", "start", Name},
			},
			Notes: []string{"log.file set to " + p.LogFile + " in " + p.ConfigPath + " (services have no stderr)", "run from an elevated prompt", "mount after login with: tailraid mount T:"},
		}, nil
	}
	return Plan{}, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
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
			append(sudo, "rm", "-f", "/etc/systemd/system/tailraid.service"),
			append(sudo, "systemctl", "daemon-reload"),
		}}, nil
	case "windows":
		return Plan{Commands: [][]string{{"sc.exe", "stop", Name}, {"sc.exe", "delete", Name}}}, nil
	}
	return Plan{}, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
}

// Start and Stop return the manager commands.
func Start(p Params) ([]string, error) {
	switch p.GOOS {
	case "darwin":
		return []string{"launchctl", "kickstart", fmt.Sprintf("gui/%d/app.tailraid.serve", p.UID)}, nil
	case "linux":
		return append(sudoPrefix(p), "systemctl", "start", Name), nil
	case "windows":
		return []string{"sc.exe", "start", Name}, nil
	}
	return nil, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
}

func Stop(p Params) ([]string, error) {
	switch p.GOOS {
	case "darwin":
		return []string{"launchctl", "kill", "SIGTERM", fmt.Sprintf("gui/%d/app.tailraid.serve", p.UID)}, nil
	case "linux":
		return append(sudoPrefix(p), "systemctl", "stop", Name), nil
	case "windows":
		return []string{"sc.exe", "stop", Name}, nil
	}
	return nil, fmt.Errorf("svc: unsupported OS %q", p.GOOS)
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
	rw := []string{p.DataDir, filepath.Dir(p.ConfigPath)}
	rw = append(rw, p.Disks...)
	if p.LogFile != "" {
		rw = append(rw, filepath.Dir(p.LogFile))
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
	for _, n := range pl.Notes {
		fmt.Fprintf(w, "# %s\n", n)
	}
}

// Apply writes the files and runs the commands, echoing each; a failing "bootout" or
// "stop" before install is tolerated (nothing was loaded yet).
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
		cmd := exec.CommandContext(ctx, c[0], c[1:]...) //nolint:gosec // the plan's commands are launchctl, systemctl and sc.exe (docs/12 §1)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = w, w, os.Stdin
		if err := cmd.Run(); err != nil {
			tolerated := i == 0 && (c[1] == "bootout" || c[1] == "stop")
			if tolerated {
				continue
			}
			if _, ok := errors.AsType[*exec.ExitError](err); ok {
				return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
			}
			return err
		}
	}
	for _, n := range pl.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	return nil
}
