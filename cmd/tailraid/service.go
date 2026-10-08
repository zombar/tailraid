package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/config"
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
	p := svc.Params{GOOS: runtime.GOOS, Exe: exe, ConfigPath: path, Home: config.Home(), LogFile: cfg.LogFile()}
	if u, err := user.Current(); err == nil {
		p.User = u.Username
		p.UID, _ = strconv.Atoi(u.Uid)
		p.Root = u.Uid == "0"
	}
	if p.LogFile == "" {
		switch runtime.GOOS {
		case "darwin":
			home, _ := os.UserHomeDir()
			p.LogFile = filepath.Join(home, "Library", "Logs", "tailraid", "serve.log")
		case "windows":
			p.LogFile = filepath.Join(config.Home(), "serve.log")
		}
	}
	return p, cfg, path, nil
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
			plan, err := svc.Install(p)
			if err != nil {
				return err
			}
			if print, _ := cmd.Flags().GetBool("print"); print {
				svc.Print(cmd.OutOrStdout(), plan)
				return nil
			}
			if runtime.GOOS == "windows" && cfg.Log.File == "" { // services have no stderr
				cfg.Log.File = p.LogFile
				if err := config.Save(path, cfg); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "set log.file: %s in %s\n", p.LogFile, path)
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
		Short: "Stop and remove the service (data and config stay)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, _, _, err := svcParams(cmd)
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
			return svc.Apply(cmd.Context(), cmd.OutOrStdout(), plan)
		},
	}
	c.Flags().Bool("print", false, "show the commands without running them")
	return c
}

func startStopCmd(name string, pick func(svc.Params) ([]string, error)) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: name + " the installed service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, _, _, err := svcParams(cmd)
			if err != nil {
				return err
			}
			c, err := pick(p)
			if err != nil {
				return err
			}
			return svc.Apply(cmd.Context(), cmd.OutOrStdout(), svc.Plan{Commands: [][]string{c}})
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
	f, err := os.Open(path)
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
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		for {
			line, err := bufio.NewReader(f).ReadString('\n')
			if line != "" {
				fmt.Fprint(w, line)
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
	}
}
