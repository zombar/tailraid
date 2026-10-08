// Package node wires a running tailraid node: tailnet identity, Raft, node RPC, shard
// store, overlay, flusher, reader, filesystem and the NFS gateway (docs/00).
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/dev"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mount"
	"github.com/zombar/tailraid/internal/nfsgw"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/tailscale"
	"github.com/zombar/tailraid/internal/vfs"
	"github.com/zombar/tailraid/internal/webui"
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
	var faults *dev.Faults // /dev/fault rules, dev.enabled only (docs/12 §3)
	var raftFaults cluster.Faults
	if cfg.Dev.Enabled {
		faults = dev.NewFaults()
		raftFaults = faults
	}
	node, err := cluster.Open(cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, ClusterID: clusterID, RaftAddr: raftAddr, Logger: log, Faults: raftFaults})
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
	clk := clock.Real()
	store, pinned, err := openStore(cfg)
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
		return rpc.Health{NodeID: id, Version: version, Applied: node.AppliedIndex(), Leader: node.IsLeader(), Used: used, Capacity: capacity, Free: max(capacity-used, 0), Dev: cfg.Dev.Enabled}
	}
	var ring *meta.EventRing // chunk history, dev.enabled only (docs/11 §7)
	var devHandler http.Handler
	var blocked func(string) bool
	if cfg.Dev.Enabled {
		ring = meta.NewEventRing(clk, selfIdx)
		devHandler = dev.Handler(node.FSM, store, faults, log)
		client.WrapTransport(faults.Transport)
		blocked = faults.Blocked
	}
	rd := vfs.NewReader(node.FSM, prober, client, cache, nil)
	rd.Events = ring
	fl := vfs.NewFlusher(ov, node.FSM, node, prober, client, rd, cache, clk, vfs.DefaultConfig(), log)
	fl.Events = ring
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if runtime.GOOS == "windows" {
		uid, gid = 0, 0
	}
	fs := vfs.New(node.FSM, node, ov, rd, clk, uid, gid)
	fs.SetHealth(func() bool { return staleFor(node, prober, clk) })

	rcfg := repair.DefaultConfig()
	if rcfg.Grace, rcfg.FillAfter, err = cfg.RepairDurations(); err != nil {
		return err
	}
	client.SetGrace(rcfg.Grace)
	prober.SetHoldDowns(rcfg.FillAfter, cluster.EvictAfter)
	repairer := repair.New(node.FSM, node, prober, client, rd, clk, rcfg, log)
	repairer.Events = ring
	gc := repair.NewGC(node.FSM, node, client, prober, clk, rcfg.Grace, log)
	gc.Events = ring
	ops := &adminOps{node: node, prober: prober, repairer: repairer, gc: gc, store: store, client: client, rd: rd, ring: ring, self: selfIdx, clk: clk, log: log, pinned: pinned}
	auth := &rpc.WhoIsAuthorizer{TS: ts, Member: node.FSM.NodeByAddr, Clk: clk}
	webAddr := "" // the browser UI listener (docs/13); listen.web 0 turns it off
	if cfg.Listen.Web != 0 {
		webAddr = net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Web))
	}
	status := func() any {
		rep := admin.Build(node, prober, clusterID, clk, repairer)
		rep.Gateway = admin.GatewayFromDirty(ov.Dirty())
		if webAddr != "" {
			rep.WebURL = "http://" + webAddr + "/"
		}
		return rep
	}
	srv, err := rpc.NewServer(rpc.Options{NodeID: id, ClusterID: clusterID, Version: version, Store: store, Applier: node, Fence: fence{node, selfIdx},
		Joiner: node, Admin: ops, Auth: auth, Members: node.FSM.Nodes, JoinToken: func() string { return node.FSM.Meta().JoinToken },
		Status: status, Clk: clk, Logger: log, MaxShard: int64(vol.ChunkSize) + 4096,
		Grace: rcfg.Grace, BadSlot: repairer.ReportBad, Dev: devHandler, Blocked: blocked})
	if err != nil {
		return err
	}
	var web *webui.Server
	webUsers := "members only"
	if webAddr != "" {
		login := selfLogin(ctx, ts, ident.IP, log)
		if login != "" {
			webUsers = login
		}
		web, err = webui.New(webui.Options{FS: fs, Store: node.FSM, Auth: auth, Login: login, Status: status,
			Chunks: func(ctx context.Context, sel string, offset, limit int) (int, []admin.ChunkReport, error) {
				return admin.DescribeChunksPage(ctx, node.FSM, prober, client, ring, sel, offset, limit)
			}, Clk: clk, Logger: log})
		if err != nil {
			return err
		}
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
	var wl net.Listener
	if web != nil {
		if wl, err = net.Listen("tcp", webAddr); err != nil {
			_ = rl.Close()
			_ = nl.Close()
			return fmt.Errorf("web ui listener: %w", err)
		}
	}
	nfsgw.UseLogger(log)           // one log stream, one format (the library logs client noise)
	if runtime.GOOS == "windows" { // mount.exe finds the gateway through the portmapper (docs/12 §2)
		_, nfsPort, _ := net.SplitHostPort(nl.Addr().String())
		port, _ := strconv.Atoi(nfsPort)
		pm := &mount.Portmap{Port: uint32(port), Log: log}
		go func() {
			if err := pm.Serve(ctx, "127.0.0.1:111"); err != nil {
				log.Error("portmap stub failed; Windows mount.exe cannot find the gateway", "err", err)
			}
		}()
	}
	log.Info("tailraid serving", "version", version, "node", id, "idx", selfIdx, "tailnet", ident.IP, "rpc", rl.Addr(), "raft", raftAddr,
		"nfs", nl.Addr(), "data", cfg.Node.DataDir, "policy", fmt.Sprintf("%d+%d", vol.K, vol.M))
	prog, args, _ := mount.Command(runtime.GOOS, nl.Addr().String(), cfg.Mount.Path)
	log.Info("mount with", "cmd", prog+" "+strings.Join(args, " "))
	if web != nil {
		log.Info("web ui", "url", "http://"+wl.Addr().String()+"/", "users", webUsers)
	}

	// background loops: RPC server, prober, address watch, flusher, repairer, maintenance, web UI
	bg, cancelBg := context.WithCancel(context.Background())
	loops := 6
	if web != nil {
		loops++
	}
	done := make(chan struct{}, loops)
	// an address change re-registers the node, then ends this process: the RPC and Raft
	// listeners are bound to the old address and the service manager restarts us
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var addrChanged atomic.Bool
	go func() { _ = srv.Serve(bg, rl); done <- struct{}{} }()
	go func() { prober.Run(bg); done <- struct{}{} }()
	go func() {
		if watchAddress(bg, ts, node, client, selfIdx, ident.IP, log) {
			addrChanged.Store(true)
			stop()
		}
		done <- struct{}{}
	}()
	go func() { fl.Run(bg); done <- struct{}{} }()
	go func() {
		repairer.Run(bg, func() bool { return node.IsLeader() && node.Health(clk, prober).State == cluster.Healthy })
		done <- struct{}{}
	}()
	go func() { ops.maintenance(bg, rcfg); done <- struct{}{} }()
	if web != nil {
		go func() { _ = web.Serve(bg, wl); done <- struct{}{} }()
	}

	err = nfsgw.Serve(sctx, nl, nfsgw.New(fs, node.FSM))
	cancelBg()
	for i := 0; i < loops; i++ {
		<-done
	}
	if addrChanged.Load() {
		return ErrAddressChanged
	}
	return err
}

// selfLogin is this node's tailnet login for the web UI's same-user rule (docs/13 §2):
// "" (members only) when WhoIs cannot answer for the node's own address or it is tagged.
func selfLogin(ctx context.Context, ts tailscale.Client, ip netip.Addr, log *slog.Logger) string {
	w, err := ts.WhoIs(ctx, ip)
	if err != nil || w.Login() == "" {
		log.Warn("web ui: own tailnet login unknown; only cluster members may open it", "err", err)
		return ""
	}
	return w.Login()
}

// staleFor reports whether the gateway has had no leader contact for the Stale threshold
// (docs/02 §Health): the Stale state, or no leader at all for that long. A short election
// is not stale, so metadata mutations wait for it instead of failing.
func staleFor(node *cluster.Node, prober *cluster.Prober, clk clock.Clock) bool {
	switch h := node.Health(clk, prober); h.State {
	case cluster.Stale:
		return true
	case cluster.NoLeader:
		return clk.Now().Sub(node.Raft.LastContact()) >= cluster.StaleThreshold
	}
	return false
}

// ErrAddressChanged is returned by Serve after the node's Tailscale address changed and
// was re-registered: the listeners are bound to the old address, so the process exits for
// the service manager to restart it (docs/05, L-99).
var ErrAddressChanged = errors.New("node: tailscale address changed and re-registered; restart to bind the new address")

// adminOps implements rpc.Admin and runs the periodic GC and sweep.
type adminOps struct {
	node     *cluster.Node
	prober   *cluster.Prober
	repairer *repair.Repairer
	gc       *repair.GC
	store    *chunk.Store
	client   *rpc.Client
	rd       *vfs.Reader
	ring     *meta.EventRing
	self     meta.NodeIdx
	clk      clock.Clock
	log      *slog.Logger
	pinned   bool // node.capacity is set: never re-derive the budget
}

// openStore opens the shard store with its budget: node.capacity pins it, otherwise it is
// derived from the data disk (docs/01 §Node budget, I-74).
func openStore(cfg config.Config) (*chunk.Store, bool, error) {
	pinned := cfg.Node.Capacity != ""
	var capBytes uint64
	if pinned {
		var err error
		if capBytes, err = config.ParseBytes(cfg.Node.Capacity); err != nil {
			return nil, false, err
		}
	}
	store, err := chunk.Open(sysfs.OS{}, filepath.Join(cfg.Node.DataDir, "shards"), int64(capBytes))
	if err != nil {
		return nil, false, err
	}
	if !pinned {
		if _, err := store.Rebudget(); err != nil {
			return nil, false, err
		}
	}
	return store, pinned, nil
}

// Budget is the shard budget init and join register for this device.
func Budget(cfg config.Config) (uint64, error) {
	store, _, err := openStore(cfg)
	if err != nil {
		return 0, err
	}
	return uint64(store.Capacity()), nil
}

// rebudget re-derives this node's budget from its data disk and registers it through the
// leader when it moved past the hysteresis (docs/01 §Node budget, I-74).
func (a *adminOps) rebudget(ctx context.Context) {
	b, err := a.store.Rebudget()
	if err != nil {
		a.log.Warn("budget: measuring the data disk failed", "err", err)
		return
	}
	nd, ok := a.node.FSM.Node(a.self)
	if !ok || !chunk.BudgetChanged(nd.Capacity, b) {
		return
	}
	op := rpc.MemberOp{Op: "set_capacity", NodeIdx: a.self, Capacity: b}
	if a.node.IsLeader() {
		err = a.node.MemberOp(ctx, op)
	} else if addr := a.node.LeaderRPCAddr(); addr != "" {
		err = a.client.Members(ctx, addr, op)
	} else {
		err = cluster.ErrNoLeader
	}
	if err != nil {
		a.log.Warn("budget: update failed, will retry", "err", err)
		return
	}
	a.log.Info("budget re-derived", "was", nd.Capacity, "now", b)
}

// setPolicy is `volume set-redundancy`: --auto hands the policy to the leader's repair
// passes (docs/03); --k/--m pin it, after the usual live-count check.
func (a *adminOps) setPolicy(ctx context.Context, op rpc.AdminOp) (any, error) {
	now := a.clk.Now().UnixNano()
	cmds := []meta.Command{{Op: meta.OpSetVolumeMode, SetVolumeMode: &meta.SetVolumeMode{Auto: op.Auto, Now: now}}}
	if !op.Auto {
		if err := meta.PolicyAllowed(a.node.FSM.Nodes(), a.prober, op.K, op.M); err != nil {
			return nil, err
		}
		cmds = append(cmds, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: op.K, M: op.M, Now: now}})
	}
	for _, c := range cmds {
		res, index, err := a.node.Propose(ctx, c)
		if err == nil {
			err = res.Err
		}
		if err != nil {
			return nil, err
		}
		if err := a.node.FSM.WaitApplied(ctx, index); err != nil {
			return nil, err
		}
	}
	return a.node.FSM.Volume(), nil
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
	budgetT := a.clk.NewTimer(cfg.GCInterval)
	defer gcT.Stop()
	defer sweepT.Stop()
	defer budgetT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-budgetT.C():
			if !a.pinned {
				a.rebudget(ctx)
			}
			budgetT.Reset(cfg.GCInterval)
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

func (a *adminOps) AdminOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	switch op.Op {
	case "repair_now", "check", "set_policy", "gc_now", "rotate_token":
		if !a.node.IsLeader() {
			if a.node.LeaderRPCAddr() == "" {
				return nil, cluster.ErrNoLeader
			}
			return nil, cluster.ErrNotLeader
		}
	}
	switch op.Op {
	case "chunk":
		return admin.DescribeChunks(ctx, a.node.FSM, a.prober, a.client, a.ring, op.Chunk)
	case "repair_now":
		return a.repairer.Scan(ctx)
	case "gc_now":
		return a.gc.RunOnce(ctx)
	case "rotate_token":
		// a MetaSet with the token only: the cluster ID is left as it is (docs/08 §Join)
		token := meta.NewJoinToken()
		res, index, err := a.node.Propose(ctx, meta.Command{Op: meta.OpMetaSet, MetaSet: &meta.MetaSet{JoinToken: token}})
		if err == nil {
			err = res.Err
		}
		if err != nil {
			return nil, err
		}
		if err := a.node.FSM.WaitApplied(ctx, index); err != nil {
			return nil, err
		}
		return map[string]string{"join_token": token}, nil
	case "set_policy":
		return a.setPolicy(ctx, op)
	case "check":
		// --scrub runs one full-verification pass through the leader's repairer, so it
		// uses the configured fill_after and rate limit and never overlaps a background pass
		var st repair.Stats
		if op.Scrub {
			var err error
			if st, err = a.repairer.Scrub(ctx); err != nil {
				return nil, err
			}
		} else {
			st, _ = a.repairer.Last()
		}
		return admin.Check(a.node.FSM, a.prober, a.repairer.Config().FillAfter, st, op.Files), nil
	case "fsck":
		snap, err := a.node.FSM.Snapshot()
		if err != nil {
			return nil, err
		}
		fs := snap.(interface{ State() *meta.State })
		rep := admin.FsckReport{Violations: meta.CheckInvariants(fs.State())}
		_ = a.store.Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error {
			h, kk, mm, idx, err := chunk.ParseKey(k)
			if err != nil {
				rep.OrphanShards++ // a file in the shard tree that is not a shard: junk the sweep will not touch
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

// watchAddress re-registers this node when its Tailscale IP changes (docs/02 §IP change)
// and returns true once the new address is in the node table, so the caller can restart.
func watchAddress(ctx context.Context, ts tailscale.Client, node *cluster.Node, client *rpc.Client, self meta.NodeIdx, current netip.Addr, log *slog.Logger) bool {
	t := time.NewTicker(cluster.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
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
		log.Warn("address re-registered; exiting so the service manager restarts the listeners on it", "addr", ip)
		return true
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
	capBytes, err := Budget(cfg)
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
