// Package node wires a running tailraid node: metadata, shard store, overlay, flusher
// and the NFS gateway (docs/00 architecture).
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gurgeh/tailraid/internal/chunk"
	"github.com/gurgeh/tailraid/internal/clock"
	"github.com/gurgeh/tailraid/internal/cluster"
	"github.com/gurgeh/tailraid/internal/config"
	"github.com/gurgeh/tailraid/internal/mount"
	"github.com/gurgeh/tailraid/internal/nfsgw"
	"github.com/gurgeh/tailraid/internal/sysfs"
	"github.com/gurgeh/tailraid/internal/vfs"
)

// Serve wires the M1 single-node stack and runs it until ctx is done. The M2 cluster
// package replaces the local peers/shard views with the tailnet ones.
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger, version string) error {
	id, err := cluster.LoadOrCreateNodeID(cfg.Node.DataDir)
	if err != nil {
		return err
	}
	node, err := cluster.Open(cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, RaftAddr: fmt.Sprintf("127.0.0.1:%d", cfg.Listen.Raft), Logger: log})
	if err != nil {
		return err
	}
	defer node.Close()
	if err := node.WaitLeader(ctx, 30*time.Second); err != nil {
		return fmt.Errorf("waiting for raft leader: %w", err)
	}
	selfIdx, ok := node.SelfIdx()
	if !ok {
		return errors.New("this node is not in the cluster node table")
	}
	capBytes, err := config.ParseBytes(cfg.Node.Capacity)
	if err != nil {
		return err
	}
	clk := clock.Real()
	store, err := chunk.Open(sysfs.OS{}, filepath.Join(cfg.Node.DataDir, "shards"), int64(capBytes))
	if err != nil {
		return err
	}
	cache, err := chunk.OpenCache(sysfs.OS{}, filepath.Join(cfg.Node.DataDir, "cache"), 1<<30, clk)
	if err != nil {
		return err
	}
	vol := node.FSM.Volume()
	ov, err := vfs.OpenOverlay(filepath.Join(cfg.Node.DataDir, "staging"), vol.ChunkSize, clk)
	if err != nil {
		return err
	}
	defer ov.Close()
	peers := cluster.LocalPeers{Idx: selfIdx}
	shards := cluster.LocalShards{Idx: selfIdx, Store: store}
	rd := vfs.NewReader(node.FSM, peers, shards, cache, nil)
	fl := vfs.NewFlusher(ov, node.FSM, node, peers, shards, rd, cache, clk, vfs.DefaultConfig(), log)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if runtime.GOOS == "windows" {
		uid, gid = 0, 0
	}
	fs := vfs.New(node.FSM, node, ov, rd, clk, uid, gid)
	l, err := nfsgw.Listen(cfg.Listen.NFS)
	if err != nil {
		return err
	}
	log.Info("tailraid serving", "version", version, "node", id, "nfs", l.Addr().String(), "data", cfg.Node.DataDir, "policy", fmt.Sprintf("%d+%d", vol.K, vol.M))
	prog, args, _ := mount.Command(runtime.GOOS, l.Addr().String(), cfg.Mount.Path)
	log.Info("mount with", "cmd", prog+" "+strings.Join(args, " "))

	fctx, fcancel := context.WithCancel(context.Background())
	flushDone := make(chan struct{})
	go func() { fl.Run(fctx); close(flushDone) }()
	err = nfsgw.Serve(ctx, l, nfsgw.New(fs, node.FSM))
	fcancel() // Run flushes everything dirty before returning
	<-flushDone
	return err
}
