package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mount"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/svc"
	"github.com/zombar/tailraid/internal/update"
)

func loadConfig(cmd *cobra.Command) (config.Config, string, error) {
	path, _ := cmd.Flags().GetString("config")
	if path == "" {
		path = config.DefaultPath()
	}
	c, err := config.Load(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, path, err
	}
	return c, path, nil
}

func initCmd() *cobra.Command {
	var k, m int
	var chunkSize, name, tokenFlag string
	c := &cobra.Command{
		Use:   "init",
		Short: "Bootstrap a new cluster on this node",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, path, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if name != "" {
				cfg.Node.Name = name
			}
			cfg.Node.Voter = true
			cs, err := config.ParseBytes(chunkSize)
			if err != nil || cs == 0 || cs > meta.MaxChunkSize {
				return fmt.Errorf("bad --chunk %q (1 byte to %d MiB)", chunkSize, meta.MaxChunkSize>>20)
			}
			if _, _, err := policyFlags(k, m); err != nil {
				return err
			}
			cfg.EnsureSMBPassword() // the share password, when this device serves SMB (docs/15 §6)
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			id, err := cluster.LoadOrCreateNodeID(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			_, ident, err := node.Tailnet(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			token := tokenFlag
			if token == "" {
				token = meta.NewJoinToken()
			}
			// --k/--m pin a manual policy (test rigs); otherwise the leader grows it as disks join (docs/03)
			auto := !cmd.Flags().Changed("k") && !cmd.Flags().Changed("m")
			raftAddr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Raft))
			n, err := cluster.Bootstrap(cmd.Context(), cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, RaftAddr: raftAddr},
				meta.Node{ // no budget: a device stores nothing, its disks do (docs/14 §9)
					Name: cfg.Node.Name, Kind: meta.KindDevice, TSStableID: ident.StableID, Addr: ident.IP.String(), Voter: true,
					RaftPort: uint16(cfg.Listen.Raft), RPCPort: uint16(cfg.Listen.RPC), Version: version, JoinedAt: time.Now().UnixNano(),
				},
				meta.SetVolume{ChunkSize: uint32(cs), K: uint8(k), M: uint8(m), Now: time.Now().UnixNano()}, auto, token)
			if err != nil {
				return err
			}
			defer n.Close()
			// disks already listed in the config become targets of this node (docs/14 §2)
			cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			disks, err := node.RegisterDisks(cmd.Context(), n, cfg, cid)
			if err != nil {
				return err
			}
			if err := node.Snapshot(cmd.Context(), n); err != nil { // the first serve restores the table from it (I-135)
				return fmt.Errorf("snapshot after init: %w", err)
			}
			mode := "manual"
			if auto {
				mode = "auto, grows as disks join"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialised cluster in %s (node %s at %s, policy %d+%d (%s), chunk %s)\nconfig: %s\njoin from another device: tailraid join %s %s\n",
				cfg.Node.DataDir, id, ident.IP, k, m, mode, chunkSize, path, ident.IP, token)
			printDisks(cmd.OutOrStdout(), cfg, disks)
			return nil
		},
	}
	c.Flags().IntVar(&k, "k", 1, "data shards per chunk (pins a manual policy)")
	c.Flags().IntVar(&m, "m", 0, "parity shards per chunk (pins a manual policy)")
	_ = c.Flags().MarkHidden("k")
	_ = c.Flags().MarkHidden("m")
	c.Flags().StringVar(&chunkSize, "chunk", "1M", "chunk size (fixed for the volume's life)")
	c.Flags().StringVar(&name, "name", "", "node name (default: hostname)")
	c.Flags().StringVar(&tokenFlag, "token", "", "join token (default: random; set for automation)")
	return c
}

func serveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the node: metadata, shard store and NFS gateway",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, path, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go exitOnSecondSignal(ctx, cmd.ErrOrStderr())
			log, lf := cfg.OpenLogger()
			defer func() { _ = lf.Close() }()
			slog.SetDefault(log)
			for _, w := range cfg.Warnings(path) {
				log.Warn(w)
			}
			go reopenLogOnSignal(ctx, lf, log)
			forget := time.AfterFunc(time.Minute, func() { forgetOldBinary(log) }) // the update's .old, once this start has held (I-128)
			defer forget.Stop()
			// the error goes to the log too: a service has no terminal, and on Windows no
			// stderr, so the reason serve stopped would be lost (docs/12 §1)
			run := func(ctx context.Context) error {
				err := node.Serve(ctx, cfg, log, version)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Error("serve stopped", "err", err)
				}
				return err
			}
			if asService, _ := cmd.Flags().GetBool("service"); asService {
				return runAsService(ctx, svc.Name, run)
			}
			return run(ctx)
		},
	}
	c.Flags().Bool("service", false, "run under the Windows service control manager (set by `tailraid install`)")
	return c
}

// forgetOldBinary removes the <exe>.old an update left once this binary has served for
// a minute: the rollback window closes with a clean start (docs/12 §5).
func forgetOldBinary(log *slog.Logger) {
	exe, err := executable()
	if err != nil {
		return
	}
	if _, err := os.Stat(exe + ".old"); err != nil {
		return
	}
	if err := update.ForgetOld(exe); err != nil {
		log.Warn("previous binary not removed", "err", err)
		return
	}
	log.Info("previous binary removed after a clean start", "path", exe+".old")
}

// exitOnSecondSignal ends the process at once when a second interrupt arrives while the
// first is still being honoured (the final flush, the leadership hand-over): an operator
// who presses Ctrl-C twice means now (docs/12 §1, I-123).
func exitOnSecondSignal(ctx context.Context, w io.Writer) {
	<-ctx.Done()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Fprintln(w, "tailraid: second signal, exiting now")
	os.Exit(130)
}

func mountCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mount [path]",
		Short: "Mount the running gateway (the OS NFS client on macOS and Linux, the SMB client on Windows)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			target := cfg.Mount.Path
			if len(args) == 1 {
				target = args[0]
			}
			if wait, _ := cmd.Flags().GetDuration("wait"); wait > 0 { // the logon task runs before the service may be up (I-142)
				addr := cfg.Listen.SMB
				if cfg.Listen.NFS != "" && runtime.GOOS != "windows" {
					addr = cfg.Listen.NFS
				}
				if err := mount.WaitGateway(cmd.Context(), addr, wait); err != nil {
					return err
				}
			}
			if runtime.GOOS != "windows" {
				if err := os.MkdirAll(target, 0o750); err != nil {
					return err
				}
			}
			// NFS where it is served (macOS, Linux); the SMB gateway otherwise, which is
			// Windows' only mount and an opt-in elsewhere (docs/15 §6)
			if cfg.Listen.NFS != "" && runtime.GOOS != "windows" {
				prog, margs, err := mount.Command(runtime.GOOS, cfg.Listen.NFS, target)
				if err != nil {
					return err
				}
				return mount.Run(cmd.Context(), cmd.OutOrStdout(), runtime.GOOS, prog, margs)
			}
			if cfg.Listen.SMB == "" {
				return errors.New("listen.smb is not set: Windows mounts over SMB (docs/15)")
			}
			if err := mount.CheckWindowsBuild(mount.WindowsBuild()); err != nil {
				return err
			}
			prog, margs, err := mount.SMBCommand(runtime.GOOS, cfg.Listen.SMB, target)
			if err != nil {
				return err
			}
			var env []string // Windows' New-SmbMapping reads the password from the environment; the others prompt
			if runtime.GOOS == "windows" {
				env = []string{mount.SMBPasswordEnv + "=" + cfg.SMB.Password}
			}
			if err := mount.RunWithEnv(cmd.Context(), cmd.OutOrStdout(), runtime.GOOS, prog, margs, env); err != nil {
				return err
			}
			if mount.Elevated() { // a drive belongs to the logon session that mapped it (docs/15 §6)
				fmt.Fprintf(cmd.OutOrStdout(), "note: this prompt is elevated, so Explorer does not show %s; the task %s maps it in your desktop session at every logon (tailraid install), or run tailraid mount from a prompt that is not elevated\n", target, svc.MountTask)
			}
			return nil
		},
	}
	c.Flags().Duration("wait", 0, "wait up to this long for the gateway to accept a connection before mounting (the logon task uses 5m)")
	return c
}

func umountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "umount [path]",
		Short: "Unmount the gateway",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			target := cfg.Mount.Path
			if len(args) == 1 {
				target = args[0]
			}
			prog, margs, err := mount.UnmountCommand(runtime.GOOS, target)
			if err != nil {
				return err
			}
			return mount.Run(cmd.Context(), cmd.OutOrStdout(), runtime.GOOS, prog, margs)
		},
	}
}
