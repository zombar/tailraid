// Package mount builds and runs the per-OS commands that attach the loopback NFS
// gateway to a directory or drive letter (docs/07 §Ports and mount commands).
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

// Command returns the program and arguments that mount the gateway at nfsAddr
// (host:port) onto target for the given GOOS.
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
		return "mount", []string{"-o", "anon,nolock,mtype=soft", `\\` + host + `\tailraid`, target}, nil
	}
	return "", nil, fmt.Errorf("mount: unsupported OS %q", goos)
}

// UnmountCommand returns the program and arguments that detach target.
func UnmountCommand(goos, target string) (string, []string, error) {
	switch goos {
	case "darwin", "linux":
		return "umount", []string{target}, nil
	case "windows":
		return "umount", []string{target}, nil
	}
	return "", nil, fmt.Errorf("mount: unsupported OS %q", goos)
}

// WindowsPrerequisites lists what a Windows machine needs before mounting.
const WindowsPrerequisites = `Windows needs the NFS client (Pro/Enterprise/Education) over TCP:
  Enable-WindowsOptionalFeature -Online -FeatureName ServicesForNFS-ClientOnly
  nfsadmin client localhost config protocol=tcp
and AnonymousUid/AnonymousGid (DWORD) under
  HKLM\SOFTWARE\Microsoft\ClientForNFS\CurrentVersion\Default
matching the uid/gid the gateway reports. See docs/windows-checklist.md.`

// Run executes prog with args, printing the command first. On Unix an EPERM from a
// non-root user is retried through sudo so the user sees exactly what is elevated.
func Run(ctx context.Context, out io.Writer, goos, prog string, args []string) error {
	fmt.Fprintf(out, "+ %s %s\n", prog, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, prog, args...) //nolint:gosec // the mount helper runs the OS mount command the caller chose (docs/07)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, os.Stdin
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if goos != "windows" && errors.As(err, &ee) && os.Geteuid() != 0 {
		fmt.Fprintf(out, "+ sudo %s %s\n", prog, strings.Join(args, " "))
		cmd = exec.CommandContext(ctx, "sudo", append([]string{prog}, args...)...) //nolint:gosec // as Run
		cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, os.Stdin
		return cmd.Run()
	}
	if goos == "windows" && errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintln(out, WindowsPrerequisites)
	}
	return err
}
