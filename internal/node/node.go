// Package node wires a running tailraid node: tailnet identity, Raft, node RPC, shard
// store, overlay, flusher, reader, filesystem and the NFS gateway (docs/00).
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mount"
	"github.com/zombar/tailraid/internal/nfsgw"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/tailscale"
	"github.com/zombar/tailraid/internal/vfs"
)

// Identity is what the tailnet says about this machine.
type Identity struct {
	IP       netip.Addr
	StableID string
	Host     string
}

// Tailnet resolves this node's identity, using a fake when dev.enabled points at one.
func Tailnet(ctx context.Context, cfg config.Config) (tailscale.Client, Identity, error) {
	var ts tailscale.Client
	if cfg.Dev.Enabled && cfg.Dev.TailscaleFake != "" {
		f, err := tailscale.LoadFake(cfg.Dev.TailscaleFake)
		if err != nil {
			return nil, Identity{}, err
		}
		ts = f
	} else {
		path, err := tailscale.Find(cfg.Tailscale.CLI)
		if err != nil {
			return nil, Identity{}, err
		}
		ts = &tailscale.CLI{Path: path}
	}
	st, err := ts.Status(ctx)
	if err != nil {
		return nil, Identity{}, err
	}
	ip, err := tailscale.SelfIPv4(st)
	if err != nil {
		return nil, Identity{}, err
	}
	return ts, Identity{IP: ip, StableID: st.Self.ID, Host: st.Self.HostName}, nil
}

// WaitTailnet retries Tailnet until tailscaled reports Running or the deadline passes
// (service start order at boot).
func WaitTailnet(ctx context.Context, cfg config.Config, timeout time.Duration, log *slog.Logger) (tailscale.Client, Identity, error) {
	deadline := time.Now().Add(timeout)
	for {
		ts, id, err := Tailnet(ctx, cfg)
		if err == nil {
			return ts, id, nil
		}
		if time.Now().After(deadline) || errors.Is(err, tailscale.ErrCLIMissing) {
			return nil, Identity{}, err
		}
		log.Info("waiting for tailscale", "err", err)
		select {
		case <-ctx.Done():
			return nil, Identity{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Serve runs the node until ctx is done.
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger, version string) error {
	id, err := cluster.LoadOrCreateNodeID(cfg.Node.DataDir)
	if err != nil {
		return err
	}
	clusterID, err := cluster.LoadClusterID(cfg.Node.DataDir)
	if err != nil {
		return fmt.Errorf("this node is not initialised (run `tailraid init` or `tailraid join`): %w", err)
	}
	ts, ident, err := WaitTailnet(ctx, cfg, 60*time.Second, log)
	if err != nil {
		return err
	}
	raftAddr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Raft))
	rpcAddr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.RPC))
	node, err := cluster.Open(cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, ClusterID: clusterID, RaftAddr: raftAddr, Logger: log})
	if err != nil {
		return err
	}
	defer node.Close()
	if err := node.WaitReady(ctx, 60*time.Second); err != nil {
		return fmt.Errorf("waiting for raft: %w", err)
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

	client := rpc.NewClient(clusterID, node.RPCAddrOf, store)
	node.SetForwarder(client)
	prober := cluster.NewProber(node, client, clk)
	prober.SelfHealth = func() rpc.Health {
		used, capacity := store.Used(), store.Capacity()
		return rpc.Health{NodeID: id, Version: version, Applied: node.AppliedIndex(), Leader: node.IsLeader(), Used: used, Capacity: capacity, Free: max(capacity-used, 0)}
	}
	rd := vfs.NewReader(node.FSM, prober, client, cache, nil)
	rd.BadSlot = func(key chunk.ShardKey, bad meta.NodeIdx) {
		if addr := node.LeaderRPCAddr(); addr != "" {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.ReportBadSlot(rctx, addr, key, bad)
		}
	}
	fl := vfs.NewFlusher(ov, node.FSM, node, prober, client, rd, cache, clk, vfs.DefaultConfig(), log)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if runtime.GOOS == "windows" {
		uid, gid = 0, 0
	}
	fs := vfs.New(node.FSM, node, ov, rd, clk, uid, gid)

	auth := &rpc.WhoIsAuthorizer{TS: ts, Members: node.FSM.Nodes, Clk: clk}
	srv, err := rpc.NewServer(rpc.Options{NodeID: id, ClusterID: clusterID, Version: version, Store: store, Applier: node, Fence: fence{node, selfIdx},
		Joiner: node, Auth: auth, Members: node.FSM.Nodes, JoinToken: func() string { return node.FSM.Meta().JoinToken },
		Status: func() any { return admin.Build(node, prober, clusterID, clk) }, Clk: clk, Logger: log, MaxShard: int64(vol.ChunkSize) + 4096})
	if err != nil {
		return err
	}
	rl, err := rpc.Listen(rpcAddr, auth)
	if err != nil {
		return err
	}
	nl, err := nfsgw.Listen(cfg.Listen.NFS)
	if err != nil {
		_ = rl.Close()
		return err
	}
	log.Info("tailraid serving", "version", version, "node", id, "idx", selfIdx, "tailnet", ident.IP, "rpc", rl.Addr(), "raft", raftAddr,
		"nfs", nl.Addr(), "data", cfg.Node.DataDir, "policy", fmt.Sprintf("%d+%d", vol.K, vol.M))
	prog, args, _ := mount.Command(runtime.GOOS, nl.Addr().String(), cfg.Mount.Path)
	log.Info("mount with", "cmd", prog+" "+strings.Join(args, " "))

	// background loops: RPC server, prober, address watch, flusher
	bg, cancelBg := context.WithCancel(context.Background())
	done := make(chan struct{}, 4)
	go func() { _ = srv.Serve(bg, rl); done <- struct{}{} }()
	go func() { prober.Run(bg); done <- struct{}{} }()
	go func() { watchAddress(bg, ts, node, client, selfIdx, ident.IP, log); done <- struct{}{} }()
	go func() { fl.Run(bg); done <- struct{}{} }()

	err = nfsgw.Serve(ctx, nl, nfsgw.New(fs, node.FSM))
	cancelBg()
	for i := 0; i < 4; i++ {
		<-done
	}
	return err
}

// fence answers the DELETE fence from the local FSM.
type fence struct {
	n    *cluster.Node
	self meta.NodeIdx
}

func (f fence) PlacedOnSelf(key chunk.ShardKey) (bool, bool) {
	h, k, m, idx, err := chunk.ParseKey(key)
	if err != nil {
		return false, false
	}
	c, ok := f.n.FSM.GetChunk(h)
	if !ok || c.K != k || c.M != m || idx >= len(c.Placement) {
		return false, ok
	}
	return c.Placement[idx] == f.self, true
}

// watchAddress re-registers this node when its Tailscale IP changes (docs/02 §IP change).
func watchAddress(ctx context.Context, ts tailscale.Client, node *cluster.Node, client *rpc.Client, self meta.NodeIdx, current netip.Addr, log *slog.Logger) {
	t := time.NewTicker(cluster.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := ts.Status(ctx)
		if err != nil {
			continue
		}
		ip, err := tailscale.SelfIPv4(st)
		if err != nil || ip == current {
			continue
		}
		log.Warn("tailscale address changed", "old", current, "new", ip)
		op := rpc.MemberOp{Op: "update_addr", NodeIdx: self, Addr: ip.String()}
		if node.IsLeader() {
			err = node.MemberOp(ctx, op)
		} else if addr := node.LeaderRPCAddr(); addr != "" {
			err = client.Members(ctx, addr, op)
		} else {
			err = cluster.ErrNoLeader
		}
		if err != nil {
			log.Warn("address update failed, will retry", "err", err)
			continue
		}
		current = ip
	}
}

// Join registers this node with an existing cluster: it starts an empty Raft so the
// leader can replicate to it, sends the join request, waits for the log, and stops.
func Join(ctx context.Context, cfg config.Config, host, token string, voter bool, version string, log *slog.Logger) (rpc.JoinResponse, error) {
	if cluster.HasState(cfg.Node.DataDir) {
		return rpc.JoinResponse{}, cluster.ErrAlreadyInit
	}
	id, err := cluster.LoadOrCreateNodeID(cfg.Node.DataDir)
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	_, ident, err := Tailnet(ctx, cfg)
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	capBytes, err := config.ParseBytes(cfg.Node.Capacity)
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	target := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		target = net.JoinHostPort(host, strconv.Itoa(rpc.DefaultRPCPort))
	}
	// learn the cluster ID from the target's health endpoint before opening the transport
	probe := rpc.NewClient([16]byte{}, nil, nil)
	h, err := probe.Health(ctx, target)
	if err != nil {
		return rpc.JoinResponse{}, fmt.Errorf("reach %s: %w", target, err)
	}
	var cid [16]byte
	if raw, err := decodeHex(h.ClusterID); err == nil && len(raw) == 16 {
		copy(cid[:], raw)
	} else {
		return rpc.JoinResponse{}, fmt.Errorf("bad cluster id from %s", target)
	}
	raftAddr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Raft))
	n, err := cluster.OpenFresh(cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, ClusterID: cid, RaftAddr: raftAddr, Logger: log})
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	defer n.Close()
	client := rpc.NewClient(cid, nil, nil)
	req := rpc.JoinRequest{Token: token, NodeID: id, Name: cfg.Node.Name, TSStableID: ident.StableID, Addr: ident.IP.String(),
		RaftPort: uint16(cfg.Listen.Raft), RPCPort: uint16(cfg.Listen.RPC), Voter: voter, Capacity: capBytes, Version: version}
	var resp rpc.JoinResponse
	for attempt := 0; attempt < 5; attempt++ {
		resp, err = client.Join(ctx, target, req)
		if err == nil {
			break
		}
		if errors.Is(err, rpc.ErrNotLeader) {
			if leader := rpc.LeaderFromErr(err); leader != "" {
				target = leader
				continue
			}
		}
		if !errors.Is(err, rpc.ErrNoLeader) && !errors.Is(err, rpc.ErrUnavailable) {
			return rpc.JoinResponse{}, err
		}
		select {
		case <-ctx.Done():
			return rpc.JoinResponse{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	if err := os.WriteFile(filepath.Join(cfg.Node.DataDir, "cluster_id"), []byte(h.ClusterID+"\n"), 0o600); err != nil {
		return resp, err
	}
	// receive the log before stopping so `serve` starts with the node table present
	if err := n.WaitReady(ctx, 60*time.Second); err != nil {
		log.Warn("joined, but the log has not fully replicated yet; serve will finish it", "err", err)
	}
	return resp, nil
}

func decodeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s)%2 != 0 {
		return nil, errors.New("odd length")
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		out[i] = byte(v)
	}
	return out, nil
}
