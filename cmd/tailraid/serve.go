package main

import (
	"context"
	"errors"
	"fmt"
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
			if k < 1 || m < 0 || k+m > 256 {
				return fmt.Errorf("bad policy k=%d m=%d", k, m)
			}
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
			budget, err := node.Budget(cfg) // pinned by node.capacity, else derived from the data disk (docs/01)
			if err != nil {
				return err
			}
			// --k/--m pin a manual policy (test rigs); otherwise the leader grows it as devices join (docs/03)
			auto := !cmd.Flags().Changed("k") && !cmd.Flags().Changed("m")
			raftAddr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Raft))
			n, err := cluster.Bootstrap(cmd.Context(), cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, RaftAddr: raftAddr},
				meta.Node{
					Name: cfg.Node.Name, Kind: meta.KindDevice, TSStableID: ident.StableID, Addr: ident.IP.String(), Voter: true, Capacity: budget,
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
			if err := node.RegisterDisks(cmd.Context(), n, cfg, cid); err != nil {
				return err
			}
			mode := "manual"
			if auto {
				mode = "auto, grows as devices join"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialised cluster in %s (node %s at %s, budget %s, policy %d+%d (%s), chunk %s)\nconfig: %s\njoin from another device: tailraid join %s %s\n",
				cfg.Node.DataDir, id, ident.IP, bytesStr(budget), k, m, mode, chunkSize, path, ident.IP, token)
			for i, d := range cfg.Node.Disks {
				fmt.Fprintf(cmd.OutOrStdout(), "disk %s: %s\n", node.DiskName(cfg.Node.Name, i+1), d.Path)
			}
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
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log, closeLog := cfg.OpenLogger()
			defer func() { _ = closeLog() }()
			slog.SetDefault(log)
			run := func(ctx context.Context) error { return node.Serve(ctx, cfg, log, version) }
			if asService, _ := cmd.Flags().GetBool("service"); asService {
				return runAsService(ctx, svc.Name, run)
			}
			return run(ctx)
		},
	}
	c.Flags().Bool("service", false, "run under the Windows service control manager (set by `tailraid install`)")
	return c
}

func mountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mount [path]",
		Short: "Mount the running gateway (uses the OS NFS client)",
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
			if runtime.GOOS != "windows" {
				if err := os.MkdirAll(target, 0o750); err != nil {
					return err
				}
			}
			prog, margs, err := mount.Command(runtime.GOOS, cfg.Listen.NFS, target)
			if err != nil {
				return err
			}
			return mount.Run(cmd.Context(), cmd.OutOrStdout(), runtime.GOOS, prog, margs)
		},
	}
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
