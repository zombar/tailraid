// Package mount builds and runs the per-OS commands that attach a loopback gateway to a
// directory or drive letter: the NFS gateway on macOS and Linux (docs/07 §Ports and mount
// commands), the SMB gateway on Windows (docs/15 §6).
package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
)

// Command returns the program and arguments that mount the NFS gateway at nfsAddr
// (host:port) onto target for the given GOOS. Windows mounts over SMB (SMBCommand).
func Command(goos, nfsAddr, target string) (string, []string, error) {
	host, port, err := net.SplitHostPort(nfsAddr)
	if err != nil {
		return "", nil, fmt.Errorf("mount: nfs address: %w", err)
	}
	switch goos {
	case "darwin":
		return "mount", []string{"-t", "nfs", "-o", fmt.Sprintf("port=%s,mountport=%s,vers=3,tcp,nolocks,soft", port, port), host + ":/", target}, nil
	case "linux":
		return "mount", []string{"-t", "nfs", "-o", fmt.Sprintf("port=%s,mountport=%s,nfsvers=3,tcp,nolock,noacl,soft", port, port), host + ":/", target}, nil
	case "windows":
		return "", nil, errors.New("mount: Windows mounts over SMB (docs/15), not NFS")
	}
	return "", nil, fmt.Errorf("mount: unsupported OS %q", goos)
}

// The share and the account the SMB gateway exports (docs/15 §4).
const (
	smbShare = "tailraid"
	smbUser  = "tailraid"
)

// SMBPasswordEnv is the environment variable the Windows mount command reads the share
// password from: a child's environment is readable by its own user and administrators
// only, unlike its argument list. `net use … *` was not an option: it reads the password
// from the console, never from a pipe, so a mount from any non-console session (SSH, a
// script) failed with "System error 86".
const SMBPasswordEnv = "TAILRAID_SMB_PASSWORD" //nolint:gosec // the variable's name, not a credential

// SMBCommand returns the program and arguments that mount the SMB gateway at smbAddr
// (host:port) onto target. On Windows it is New-SmbMapping through PowerShell, with the
// password taken from SMBPasswordEnv (RunWithEnv sets it); macOS and Linux prompt.
func SMBCommand(goos, smbAddr, target string) (string, []string, error) {
	host, port, err := net.SplitHostPort(smbAddr)
	if err != nil {
		return "", nil, fmt.Errorf("mount: smb address: %w", err)
	}
	switch goos {
	case "windows":
		return "powershell", []string{
			"-NoProfile", "-NonInteractive", "-Command",
			"New-SmbMapping", "-LocalPath", target, "-RemotePath", `\\` + host + `\` + smbShare, "-TcpPort", port,
			"-UserName", smbUser, "-Password", "$env:" + SMBPasswordEnv, "-Persistent", "$false",
		}, nil
	case "darwin":
		return "mount_smbfs", []string{fmt.Sprintf("//%s@%s:%s/%s", smbUser, host, port, smbShare), target}, nil
	case "linux":
		return "mount", []string{"-t", "cifs", "//" + host + "/" + smbShare, target, "-o", fmt.Sprintf("port=%s,username=%s,vers=3.0", port, smbUser)}, nil
	}
	return "", nil, fmt.Errorf("mount: unsupported OS %q", goos)
}

// UnmountCommand returns the program and arguments that detach target.
func UnmountCommand(goos, target string) (string, []string, error) {
	switch goos {
	case "darwin", "linux":
		return "umount", []string{target}, nil
	case "windows":
		return "net", []string{"use", target, "/delete", "/y"}, nil
	}
	return "", nil, fmt.Errorf("mount: unsupported OS %q", goos)
}

// MinWindowsBuild is Windows 11 24H2, the first whose SMB client can be told a port other
// than 445 (docs/15 §1).
const MinWindowsBuild = 26100

// CheckWindowsBuild refuses a Windows build older than MinWindowsBuild; 0 (the build is
// not known) passes.
func CheckWindowsBuild(build uint32) error {
	if build != 0 && build < MinWindowsBuild {
		return fmt.Errorf("mount: Windows 11 24H2 (build %d) or newer is required: its SMB client can connect to a port other than 445 (net use /TCPPORT); this is build %d", MinWindowsBuild, build)
	}
	return nil
}

// WindowsNotes is printed when PowerShell cannot be run: the console form of the mount.
const WindowsNotes = `Windows mounts the SMB gateway (Windows 11 24H2 or newer). Without PowerShell, from a console:
  net use T: \\127.0.0.1\tailraid /TCPPORT:20445 /USER:tailraid * /PERSISTENT:NO
and type smb.password from the config file at the prompt. See docs/15-smb-gateway.md and docs/windows-checklist.md.`

// Run executes prog with args, printing the command first. On Unix an EPERM from a
// non-root user is retried through sudo so the user sees exactly what is elevated.
func Run(ctx context.Context, out io.Writer, goos, prog string, args []string) error {
	return RunWithEnv(ctx, out, goos, prog, args, nil)
}

// RunWithEnv is Run with extra "KEY=value" entries in the command's environment: the
// Windows mount takes the share password from SMBPasswordEnv there, so it is in neither
// the printed command nor the process list.
func RunWithEnv(ctx context.Context, out io.Writer, goos, prog string, args, env []string) error {
	fmt.Fprintf(out, "+ %s %s\n", prog, strings.Join(args, " "))
	run := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the mount command this package built
		cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, os.Stdin
		if len(env) > 0 {
			cmd.Env = append(os.Environ(), env...)
		}
		return cmd.Run()
	}
	err := run(prog, args...)
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if goos != "windows" && errors.As(err, &ee) && os.Geteuid() != 0 {
		fmt.Fprintf(out, "+ sudo %s %s\n", prog, strings.Join(args, " "))
		return run("sudo", append([]string{prog}, args...)...)
	}
	if goos == "windows" && errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintln(out, WindowsNotes)
	}
	return err
}

// ElevatedWarning is what `mount` prints before mapping target from an elevated window:
// the letter will exist for elevated programs only, and Explorer runs unelevated
// (docs/15 §6). Empty when not elevated.
func ElevatedWarning(elevated bool, target string) string {
	if !elevated {
		return ""
	}
	return fmt.Sprintf("warning: this window runs as administrator, so %[1]s will be visible to elevated programs only, not to Explorer.\n"+
		"To see it in Explorer: `net use %[1]s /delete` here, then `tailraid mount` from a normal (non-administrator) window.", target)
}
