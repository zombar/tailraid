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
	"sort"
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
	"github.com/zombar/tailraid/internal/repair"
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
	fl := vfs.NewFlusher(ov, node.FSM, node, prober, client, rd, cache, clk, vfs.DefaultConfig(), log)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if runtime.GOOS == "windows" {
		uid, gid = 0, 0
	}
	fs := vfs.New(node.FSM, node, ov, rd, clk, uid, gid)

	rcfg := repair.DefaultConfig()
	repairer := repair.New(node.FSM, node, prober, client, rd, clk, rcfg, log)
	gc := repair.NewGC(node.FSM, node, client, prober, clk, rcfg.Grace, log)
	ops := &adminOps{node: node, prober: prober, repairer: repairer, gc: gc, store: store, self: selfIdx, clk: clk, log: log}
	auth := &rpc.WhoIsAuthorizer{TS: ts, Members: node.FSM.Nodes, Clk: clk}
	srv, err := rpc.NewServer(rpc.Options{NodeID: id, ClusterID: clusterID, Version: version, Store: store, Applier: node, Fence: fence{node, selfIdx},
		Joiner: node, Admin: ops, Auth: auth, Members: node.FSM.Nodes, JoinToken: func() string { return node.FSM.Meta().JoinToken },
		Status: func() any { return admin.Build(node, prober, clusterID, clk, repairer) }, Clk: clk, Logger: log, MaxShard: int64(vol.ChunkSize) + 4096,
		BadSlot: repairer.ReportBad})
	if err != nil {
		return err
	}
	rd.BadSlot = func(key chunk.ShardKey, bad meta.NodeIdx) {
		if node.IsLeader() {
			repairer.ReportBad(key, bad)
			return
		}
		if addr := node.LeaderRPCAddr(); addr != "" {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.ReportBadSlot(rctx, addr, key, bad)
		}
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
	const loops = 6
	done := make(chan struct{}, loops)
	go func() { _ = srv.Serve(bg, rl); done <- struct{}{} }()
	go func() { prober.Run(bg); done <- struct{}{} }()
	go func() { watchAddress(bg, ts, node, client, selfIdx, ident.IP, log); done <- struct{}{} }()
	go func() { fl.Run(bg); done <- struct{}{} }()
	go func() {
		repairer.Run(bg, func() bool { return node.IsLeader() && node.Health(clk, prober).State == cluster.Healthy })
		done <- struct{}{}
	}()
	go func() { ops.maintenance(bg, rcfg); done <- struct{}{} }()

	err = nfsgw.Serve(ctx, nl, nfsgw.New(fs, node.FSM))
	cancelBg()
	for i := 0; i < loops; i++ {
		<-done
	}
	return err
}

// adminOps implements rpc.Admin and runs the periodic GC and sweep.
type adminOps struct {
	node     *cluster.Node
	prober   *cluster.Prober
	repairer *repair.Repairer
	gc       *repair.GC
	store    *chunk.Store
	self     meta.NodeIdx
	clk      clock.Clock
	log      *slog.Logger
}

func (a *adminOps) healthy() bool { return a.node.Health(a.clk, a.prober).State == cluster.Healthy }

// leaderApplied reads the leader's applied index from the prober table.
func (a *adminOps) leaderApplied() (uint64, bool) {
	if a.node.IsLeader() {
		return a.node.AppliedIndex(), true
	}
	_, id := a.node.Raft.LeaderWithID()
	nodes := a.node.FSM.Nodes()
	for i := range nodes {
		if nodes[i].ID == string(id) {
			if pr, ok := a.prober.Snapshot()[nodes[i].Idx]; ok && !pr.LastOK.IsZero() {
				return pr.Applied, true
			}
		}
	}
	return 0, false
}

// maintenance runs GC (leader) every GCInterval and the local sweep hourly.
func (a *adminOps) maintenance(ctx context.Context, cfg repair.Config) {
	gcT := a.clk.NewTimer(cfg.GCInterval)
	sweepT := a.clk.NewTimer(time.Hour)
	defer gcT.Stop()
	defer sweepT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gcT.C():
			if a.node.IsLeader() && a.healthy() {
				if res, err := a.gc.RunOnce(ctx); err != nil {
					a.log.Warn("gc failed", "err", err)
				} else if res.Victims > 0 {
					a.log.Info("gc", "victims", res.Victims, "deleted", res.Deleted, "refused", res.Refused)
				}
			}
			gcT.Reset(cfg.GCInterval)
		case <-sweepT.C():
			if n, err := repair.SweepLocal(ctx, a.store, a.node.FSM, a.self, cfg.Grace, a.clk, a.healthy, a.node.AppliedIndex, a.leaderApplied); err != nil {
				a.log.Warn("sweep failed", "err", err)
			} else if n > 0 {
				a.log.Info("sweep", "orphans_removed", n)
			}
			sweepT.Reset(time.Hour)
		}
	}
}

// CheckReport is the `check` reply.
type CheckReport struct {
	Scan       repair.Stats `json:"scan"`
	Degraded   int          `json:"degraded_chunks"`
	Unreadable int          `json:"unreadable_chunks"`
	Files      []string     `json:"files,omitempty"` // paths touching degraded/unreadable chunks
	OK         bool         `json:"ok"`
}

// FsckReport is the `fsck` reply.
type FsckReport struct {
	Violations   []string `json:"violations"`
	OrphanShards int      `json:"orphan_shards_on_this_node"`
	OK           bool     `json:"ok"`
}

func (a *adminOps) AdminOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	switch op.Op {
	case "repair_now", "check", "set_policy", "gc_now":
		if !a.node.IsLeader() {
			if a.node.LeaderRPCAddr() == "" {
				return nil, cluster.ErrNoLeader
			}
			return nil, cluster.ErrNotLeader
		}
	}
	switch op.Op {
	case "repair_now":
		return a.repairer.Scan(ctx)
	case "gc_now":
		return a.gc.RunOnce(ctx)
	case "set_policy":
		if op.K < 1 || int(op.K)+int(op.M) > 256 {
			return nil, fmt.Errorf("bad policy %d+%d", op.K, op.M)
		}
		live := 0
		nodes := a.node.FSM.Nodes()
		for i := range nodes {
			if nodes[i].State == meta.Active && a.prober.IsLive(nodes[i].Idx) {
				live++
			}
		}
		if int(op.K)+int(op.M) > live {
			return nil, fmt.Errorf("policy %d+%d needs %d live active nodes, have %d", op.K, op.M, int(op.K)+int(op.M), live)
		}
		res, index, err := a.node.Propose(ctx, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: op.K, M: op.M, Now: a.clk.Now().UnixNano()}})
		if err == nil {
			err = res.Err
		}
		if err != nil {
			return nil, err
		}
		if err := a.node.FSM.WaitApplied(ctx, index); err != nil {
			return nil, err
		}
		return a.node.FSM.Volume(), nil
	case "check":
		cfg := repair.DefaultConfig()
		if op.Scrub {
			cfg.ScrubShare = 1
		}
		var st repair.Stats
		var err error
		if op.Scrub {
			st, err = repair.New(a.node.FSM, a.node, a.prober, nil, nil, a.clk, cfg, a.log).Scan(ctx)
		} else {
			st, _ = a.repairer.Last()
		}
		if err != nil {
			return nil, err
		}
		rep := CheckReport{Scan: st}
		degraded := map[meta.Hash]bool{}
		a.node.FSM.ForEachChunk(func(c meta.Chunk) bool {
			if c.Refs == 0 {
				return true
			}
			good, liveSlots := 0, 0
			for _, s := range c.Placement {
				if s != 0 {
					good++
					if a.prober.IsLive(s) {
						liveSlots++
					}
				}
			}
			if liveSlots < int(c.K)+int(c.M) {
				rep.Degraded++
				degraded[c.Hash] = true
			}
			if liveSlots < int(c.K) {
				rep.Unreadable++
			}
			return true
		})
		if op.Files && len(degraded) > 0 {
			rep.Files = a.filesReferencing(degraded)
		}
		rep.OK = rep.Unreadable == 0 && rep.Degraded == 0
		return rep, nil
	case "fsck":
		snap, err := a.node.FSM.Snapshot()
		if err != nil {
			return nil, err
		}
		fs := snap.(interface{ State() *meta.State })
		rep := FsckReport{Violations: meta.CheckInvariants(fs.State())}
		_ = a.store.Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error {
			h, kk, mm, idx, err := chunk.ParseKey(k)
			if err != nil {
				return nil
			}
			c, ok := a.node.FSM.GetChunk(h)
			if !ok || c.K != kk || c.M != mm || idx >= len(c.Placement) || c.Placement[idx] != a.self {
				rep.OrphanShards++
			}
			return nil
		})
		rep.OK = len(rep.Violations) == 0
		return rep, nil
	}
	return nil, fmt.Errorf("unknown admin op %q", op.Op)
}

// filesReferencing walks the tree once and returns the paths whose chunks are in set.
func (a *adminOps) filesReferencing(set map[meta.Hash]bool) []string {
	var out []string
	var walk func(ino meta.Ino, prefix string)
	walk = func(ino meta.Ino, prefix string) {
		ents, err := a.node.FSM.ReadDir(ino)
		if err != nil {
			return
		}
		for i := range ents {
			p := prefix + "/" + ents[i].Name
			switch ents[i].Type {
			case meta.TDir:
				walk(ents[i].Ino, p)
			case meta.TFile:
				for _, h := range ents[i].Chunks {
					if set[h] {
						out = append(out, p)
						break
					}
				}
			}
		}
	}
	walk(meta.RootIno, "")
	sort.Strings(out)
	return out
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
