package main

import (
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
	var chunkSize, capacity, name, tokenFlag string
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
			if capacity != "" {
				cfg.Node.Capacity = capacity
			}
			cfg.Node.Voter = true
			capBytes, err := config.ParseBytes(cfg.Node.Capacity)
			if err != nil {
				return err
			}
			cs, err := config.ParseBytes(chunkSize)
			if err != nil || cs == 0 || cs > 1<<30 {
				return fmt.Errorf("bad --chunk %q", chunkSize)
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
				token = randomToken()
			}
			raftAddr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Raft))
			n, err := cluster.Bootstrap(cmd.Context(), cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, RaftAddr: raftAddr},
				meta.Node{Name: cfg.Node.Name, Kind: meta.KindDevice, TSStableID: ident.StableID, Addr: ident.IP.String(), Voter: true, Capacity: capBytes,
					RaftPort: uint16(cfg.Listen.Raft), RPCPort: uint16(cfg.Listen.RPC), Version: version, JoinedAt: time.Now().UnixNano()},
				meta.SetVolume{ChunkSize: uint32(cs), K: uint8(k), M: uint8(m), Now: time.Now().UnixNano()}, token)
			if err != nil {
				return err
			}
			defer n.Close()
			fmt.Fprintf(cmd.OutOrStdout(), "initialised cluster in %s (node %s at %s, policy %d+%d, chunk %s)\nconfig: %s\njoin from another device: tailraid join %s %s\n",
				cfg.Node.DataDir, id, ident.IP, k, m, chunkSize, path, ident.IP, token)
			return nil
		},
	}
	c.Flags().IntVar(&k, "k", 1, "data shards per chunk")
	c.Flags().IntVar(&m, "m", 0, "parity shards per chunk")
	c.Flags().StringVar(&chunkSize, "chunk", "1M", "chunk size (fixed for the volume's life)")
	c.Flags().StringVar(&capacity, "capacity", "", "shard-byte budget for this node (default from config)")
	c.Flags().StringVar(&name, "name", "", "node name (default: hostname)")
	c.Flags().StringVar(&tokenFlag, "token", "", "join token (default: random; set for automation)")
	return c
}

func randomToken() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	f, err := os.Open("/dev/urandom")
	if err == nil {
		_, _ = f.Read(b)
		_ = f.Close()
	} else {
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i * 7))
		}
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the node: metadata, shard store and NFS gateway",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log := cfg.Logger()
			slog.SetDefault(log)
			return node.Serve(ctx, cfg, log, version)
		},
	}
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
				if err := os.MkdirAll(target, 0o755); err != nil {
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
