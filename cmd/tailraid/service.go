package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/keychain"
	"github.com/zombar/tailraid/internal/mesh"
	"github.com/zombar/tailraid/internal/mesh/wintun"
	"github.com/zombar/tailraid/internal/svc"
)

func svcParams(cmd *cobra.Command) (svc.Params, config.Config, string, error) {
	cfg, path, err := loadConfig(cmd)
	if err != nil {
		return svc.Params{}, cfg, "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return svc.Params{}, cfg, "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return svc.Params{}, cfg, "", err
	}
	if strings.Contains(exe, "go-build") { // `go run` binaries vanish; the unit would point at nothing
		return svc.Params{}, cfg, "", fmt.Errorf("install needs a stable binary path, not %s (make build, then copy dist/tailraid somewhere permanent)", exe)
	}
	p := svc.Params{GOOS: runtime.GOOS, Exe: exe, ConfigPath: path, Home: config.Home(), LogFile: cfg.LogFile(), DataDir: cfg.Node.DataDir, MeshPort: cfg.Mesh.Port}
	p.LogonMount = runtime.GOOS == "windows" && cfg.Listen.SMB != "" && cfg.Mount.Path != "" // the drive at logon (I-142)
	for _, d := range cfg.Node.Disks {
		p.Disks = append(p.Disks, d.Path)
	}
	for _, m := range cfg.Node.Mirrors { // a mirror is written by serve as a disk is (docs/16 §1)
		p.Disks = append(p.Disks, m.Path)
	}
	if u, err := user.Current(); err == nil {
		p.User = u.Username
		p.UID, _ = strconv.Atoi(u.Uid)
		p.Root = u.Uid == "0"
	}
	if p.LogFile == "" {
		switch runtime.GOOS {
		case "darwin":
			p.LogFile = "/Library/Logs/tailraid/serve.log" // the root daemon's (docs/20 §9)
		case "windows":
			p.LogFile = filepath.Join(config.Home(), "serve.log")
		}
	}
	return p, cfg, path, nil
}

// giveBack hands what a command run under sudo wrote back to the user who ran it, on
// Linux, where serve runs as that user (docs/20 §9): a join needs CAP_NET_ADMIN for its
// interface, so it runs under sudo and the data directory must not end up root's. On
// macOS the service runs as root and keeps what it wrote; elsewhere nothing to do.
func giveBack(paths ...string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	uid, gid, ok := sudoIDs()
	if !ok {
		return nil
	}
	for _, root := range paths {
		if root == "" {
			continue
		}
		if err := chownTree(root, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("give %s back to uid %d: %w", root, uid, err)
		}
	}
	return nil
}

// sudoIDs is the user behind a sudo; ok is false without sudo or when sudo was run by root.
func sudoIDs() (uid, gid int, ok bool) {
	var err1, err2 error
	uid, err1 = strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 = strconv.Atoi(os.Getenv("SUDO_GID"))
	return uid, gid, err1 == nil && err2 == nil && uid != 0
}

// chownTree chowns root and everything under it through os.Root, so a symlink planted
// under the data directory never leads the chown outside it.
func chownTree(root string, uid, gid int) error {
	if err := os.Lchown(root, uid, gid); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		return err
	}
	defer r.Close()
	return fs.WalkDir(r.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		return r.Lchown(p, uid, gid)
	})
}

// sudoOwner is the user behind a sudo, as mount.owner records it ("" without sudo).
func sudoOwner() string {
	uid, gid := os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID")
	if uid == "" || gid == "" || uid == "0" {
		return ""
	}
	return uid + ":" + gid
}

func installCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "install",
		Short: "Register `tailraid serve` with the OS service manager (launchd, systemd, SCM) and start it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, cfg, path, err := svcParams(cmd)
			if err != nil {
				return err
			}
			if runtime.GOOS == "linux" || runtime.GOOS == "darwin" { // the unit is staged in a directory nobody else can pre-create (I-122)
				stage, err := os.MkdirTemp("", "tailraid-install-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(stage)
				p.Stage = stage
			}
			plan, err := svc.Install(p)
			if err != nil {
				return err
			}
			if print, _ := cmd.Flags().GetBool("print"); print {
				svc.Print(cmd.OutOrStdout(), plan)
				return nil
			}
			changed := false
			if runtime.GOOS == "windows" && cfg.Log.File == "" { // services have no stderr
				cfg.Log.File, changed = p.LogFile, true
				fmt.Fprintf(cmd.OutOrStdout(), "set log.file: %s in %s\n", p.LogFile, path)
			}
			if runtime.GOOS == "darwin" && cfg.Mount.Owner == "" { // the user's ownership on the root daemon's mount (docs/20 §9)
				if owner := sudoOwner(); owner != "" {
					cfg.Mount.Owner, changed = owner, true
					fmt.Fprintf(cmd.OutOrStdout(), "set mount.owner: %s in %s\n", owner, path)
				}
			}
			if changed {
				if err := config.Save(path, cfg); err != nil {
					return err
				}
			}
			if runtime.GOOS == "windows" { // the device's driver beside the binary (docs/20 §9, I-324)
				if written, err := wintun.EnsureDLL(filepath.Dir(p.Exe)); err != nil {
					return err
				} else if written {
					fmt.Fprintf(cmd.OutOrStdout(), "wrote %s beside %s\n", wintun.Name, p.Exe)
				}
			}
			return svc.Apply(cmd.Context(), cmd.OutOrStdout(), plan)
		},
	}
	c.Flags().Bool("print", false, "show the unit file and commands without applying them")
	return c
}

func uninstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the service (data and config stay; --forget-identity also removes the device's mesh key)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, cfg, _, err := svcParams(cmd)
			if err != nil {
				return err
			}
			plan, err := svc.Uninstall(p)
			if err != nil {
				return err
			}
			if print, _ := cmd.Flags().GetBool("print"); print {
				svc.Print(cmd.OutOrStdout(), plan)
				return nil
			}
			if err := svc.Apply(cmd.Context(), cmd.OutOrStdout(), plan); err != nil {
				return err
			}
			if forget, _ := cmd.Flags().GetBool("forget-identity"); forget { // the key reset keeps (docs/20 §2, I-316)
				cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
				if err != nil {
					return fmt.Errorf("forget identity: %w", err)
				}
				if err := mesh.ForgetKey(keychain.New(cfg.Node.DataDir), cid); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "mesh key removed: the next join makes this a new device")
			}
			return nil
		},
	}
	c.Flags().Bool("print", false, "show the commands without running them")
	c.Flags().Bool("forget-identity", false, "also remove the device's mesh key from the keychain")
	return c
}

func startStopCmd(name string, pick func(svc.Params) (svc.Plan, error)) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: name + " the installed service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, _, _, err := svcParams(cmd)
			if err != nil {
				return err
			}
			plan, err := pick(p)
			if err != nil {
				return err
			}
			return svc.Apply(cmd.Context(), cmd.OutOrStdout(), plan)
		},
	}
}

func logsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "logs",
		Short: "Print or follow the service log (journald on Linux, the log file elsewhere)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, _, _, err := svcParams(cmd)
			if err != nil {
				return err
			}
			follow, _ := cmd.Flags().GetBool("follow")
			n, _ := cmd.Flags().GetInt("lines")
			if c := svc.Logs(p, follow, n); c != nil {
				return svc.Apply(cmd.Context(), cmd.OutOrStdout(), svc.Plan{Commands: [][]string{c}})
			}
			return tailFile(cmd, p.LogFile, follow, n)
		},
	}
	c.Flags().BoolP("follow", "f", false, "keep printing new lines")
	c.Flags().IntP("lines", "n", 100, "number of trailing lines to print first")
	return c
}

// tailFile prints the last n lines of path and, with follow, keeps printing appended lines.
func tailFile(cmd *cobra.Command, path string, follow bool, n int) error {
	f, err := os.Open(path) //nolint:gosec // log.file from the config
	if err != nil {
		return fmt.Errorf("log file %s: %w (is the service installed?)", path, err)
	}
	defer f.Close()
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	w := cmd.OutOrStdout()
	for _, l := range ring {
		fmt.Fprintln(w, l)
	}
	if !follow {
		return nil
	}
	rd := bufio.NewReader(f) // one reader: a fresh one per tick would drop the bytes the previous one buffered
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		for {
			line, err := rd.ReadString('\n')
			if line != "" {
				fmt.Fprint(w, line)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
		}
	}
}
