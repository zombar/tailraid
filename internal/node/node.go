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
	"sync"
	"sync/atomic"
	"time"

	smb "github.com/go-filesystems/smb"
	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/dev"
	"github.com/zombar/tailraid/internal/logx"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mount"
	"github.com/zombar/tailraid/internal/nfsgw"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/stats"
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
	DNSName  string // MagicDNS name without the trailing dot ("" when unknown)
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
	return ts, Identity{IP: ip, StableID: st.Self.ID, Host: st.Self.HostName, DNSName: strings.TrimSuffix(st.Self.DNSName, ".")}, nil
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
	// the authorizer exists before the cluster node: the Raft transport gates accepted
	// connections with it (I-92); the member table is bound as soon as the FSM exists
	auth := &rpc.WhoIsAuthorizer{TS: ts, Clk: clock.Real()}
	node, err := cluster.Open(cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, ClusterID: clusterID, RaftAddr: raftAddr, Logger: log, Faults: raftFaults, PeerAuth: auth})
	if err != nil {
		return err
	}
	defer node.Shutdown(5 * time.Second) //nolint:errcheck // a leader hands over first (I-123); the close error is logged by Close's callers elsewhere
	auth.SetMembers(node.FSM.NodeByAddr)
	// Raft restores a follower's table from a snapshot only: a node restarted before its
	// first snapshot (every fresh joiner) has an empty table until the leader connects,
	// and the leader is accepted only by that table. The seed the join persisted, kept
	// current from the table while serve runs, stands in until then (I-92).
	if seed, err := cluster.LoadMemberSeed(cfg.Node.DataDir); err != nil {
		log.Warn("member seed unreadable; the leader is accepted once the table arrives", "err", err)
	} else if len(seed) > 0 {
		auth.SetSeed(seed)
	} else if len(node.FSM.Nodes()) == 0 {
		// no table and no seed (a join from before the seed existed, or a lost file): the
		// member entries already in this node's own log name the leader (I-134)
		if members, err := node.MembersFromLog(); err != nil {
			log.Warn("members not readable from the local log", "err", err)
		} else if len(members) > 0 {
			auth.SetSeed(members)
			log.Info("no member seed: accepting the members named by the local log", "members", len(members))
		}
	}
	// A node whose snapshot holds its own row and the volume serves at once, leader or
	// not: stale gateways, ready=false, ready once a leader connects (I-135). Waiting for
	// a leader here would exit after the bound and leave the service manager restarting
	// a node that nobody can reach meanwhile. Only a node without that state (the first
	// start after a join that ended before its snapshot) needs the log first.
	fatal := make(chan error, 1) // a check that fails once the node is ready ends serve
	if _, ok := node.SelfIdx(); !ok || node.FSM.Volume().ChunkSize == 0 {
		if err := node.WaitReady(ctx, 60*time.Second); err != nil {
			return fmt.Errorf("waiting for raft: %w", err)
		}
		// a leader is known from its first heartbeat; the row comes with the log (I-144)
		if err := node.WaitMember(ctx, 60*time.Second); err != nil {
			return fmt.Errorf("waiting for this node's row: %w", err)
		}
	} else if err := node.WaitReady(ctx, 5*time.Second); err != nil { // the usual case: a leader within seconds
		log.Info("no leader yet; serving stale until one connects (status exits 1 meanwhile)")
	}
	// Raft restores a table at open from a snapshot only, so the state the next start
	// needs is snapshotted once this one is ready (and again at a clean shutdown); the
	// legacy check runs again then, on the state the log brought
	go onReady(ctx, node, log, fatal)
	if err := cluster.WriteMemberSeed(cfg.Node.DataDir, node.FSM.Nodes()); err != nil {
		log.Warn("member seed not written", "err", err)
	}
	selfIdx, ok := node.SelfIdx()
	if !ok {
		return errors.New("this node is not in the cluster node table")
	}
	if err := legacyCheck(node.FSM); err != nil { // a state from before devices stopped being targets (docs/14 §9, I-119)
		return err
	}
	clk := clock.Real()
	cacheSize, err := cfg.CacheSize()
	if err != nil {
		return err
	}
	cache, err := chunk.OpenCache(sysfs.OS{}, filepath.Join(cfg.Node.DataDir, "cache"), cacheSize, clk)
	if err != nil {
		return err
	}
	defer cache.Drain() // the writes behind the last Puts (at most CacheQueue chunks, I-204)
	vol := node.FSM.Volume()
	ov, err := vfs.OpenOverlay(filepath.Join(cfg.Node.DataDir, "staging"), vol.ChunkSize, clk, vfs.WithLog(log))
	if err != nil {
		return err
	}
	defer ov.Close()

	disks := newDiskSet(cfg, clusterID, id, selfIdx, log) // this host's disks, the only shard stores (docs/14 §3, §9)
	disks.refresh(node.FSM.Nodes())

	client := rpc.NewClient(clusterID, node.RPCAddrOf)
	client.SetDisks(node.DiskOf, disks.local)
	// S3 nodes: the buckets this device holds credentials for (docs/10 §4.1)
	s3keys := cluster.KeysFromFile(cfg.Node.DataDir, func(err error) { log.Warn("s3 credentials file unreadable; keeping the last good keys", "err", err) })
	client.SetS3(cluster.NewS3Buckets(node, s3keys, nil).Bucket)
	node.SetForwarder(client)
	prober := cluster.NewProber(node, client, clk)
	prober.SelfHealth = func() rpc.Health {
		return rpc.Health{NodeID: id, Version: version, Applied: node.AppliedIndex(), Leader: node.IsLeader(), Dev: cfg.Dev.Enabled, Web: webPort(cfg), Disks: disks.health()}
	}
	var ring *meta.EventRing // chunk history, dev.enabled only (docs/11 §7)
	var devHandler http.Handler
	var blocked func(string) bool
	if cfg.Dev.Enabled {
		ring = meta.NewEventRing(clk, selfIdx)
		devHandler = dev.Handler(node.FSM, devStores{disks: disks, fsm: node.FSM}, faults, log)
		client.WrapTransport(faults.Transport)
		blocked = faults.Blocked
	}
	client.OnUnreachable = prober.Suspect // a shard RPC that fails at the transport level (docs/05 §6, I-170)
	rd := vfs.NewReader(node.FSM, prober, client, cache, nil)
	defer rd.Close() // before cache.Drain: no fetch Puts into a draining cache (I-204)
	rd.Events = ring
	pinned, err := pins.Open(cfg.Node.DataDir, node.FSM, rd, cache, clk, log) // the folders kept on this device (docs/01 §Pins)
	if err != nil {
		return err
	}
	fl := vfs.NewFlusher(ov, node.FSM, node, prober, client, rd, clk, vfs.DefaultConfig(), log)
	fl.Events = ring
	fl.Suspect = prober.Suspect // a PUT that outlasts the straggler grace (I-169)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if runtime.GOOS == "windows" {
		uid, gid = 0, 0
	}
	fs := vfs.New(node.FSM, node, ov, rd, clk, uid, gid)
	var st staleness
	fs.SetHealth(func() bool { return st.stale(node.Health(clk, prober).State, node.Raft.LastContact(), clk.Now()) })

	rcfg := repair.DefaultConfig()
	if rcfg.Grace, rcfg.FillAfter, err = cfg.RepairDurations(); err != nil {
		return err
	}
	if rcfg.Retention, err = cfg.TrashRetention(); err != nil {
		return err
	}
	if rcfg.S3ScrubBytes, err = cfg.S3ScrubBytes(); err != nil {
		return err
	}
	workers, err := cfg.RepairWorkers()
	if err != nil {
		return err
	}
	if workers > 0 {
		rcfg.Workers = workers
	}
	rate, rateSet, err := cfg.RepairRateBytes()
	if err != nil {
		return err
	}
	if rateSet {
		rcfg.RateBytes = rate
	}
	scrubPeriod, scrubSet, err := cfg.RepairScrubPeriod()
	if err != nil {
		return err
	}
	if scrubSet {
		rcfg.ScrubPeriod = scrubPeriod
	}
	client.SetGrace(rcfg.Grace)
	prober.SetHoldDowns(rcfg.FillAfter, cluster.EvictAfter)
	repairer := repair.New(node.FSM, node, prober, client, rd, clk, rcfg, log)
	repairer.Events = ring
	gc := repair.NewGC(node.FSM, node, client, prober, clk, rcfg.Grace, log)
	gc.Events = ring
	gc.Retention = rcfg.Retention
	node.MinCodec = prober.MinCodec // the trash and the flags wait until every live member reads codec 4 (docs/12 §6)
	fs.SetFlusher(fl)
	fl.MinCodec = prober.MinCodec // a Commit carries shard sums once every live member reads codec 5 (I-201)
	// the stats history (docs/17): this node's rows every stats.every, the cluster's while
	// it leads, sealed into the volume at the hour; its reader has no chunk cache (I-196)
	statsEvery, err := cfg.StatsEvery()
	if err != nil {
		return err
	}
	keepRaw, keepMinute, keepCoarse, err := cfg.StatsKeep()
	if err != nil {
		return err
	}
	statsSvc := &stats.Service{
		Name: cfg.Node.Name, Every: statsEvery, Keep: stats.Keep{Raw: keepRaw, Minute: keepMinute, Coarse: keepCoarse}, Clk: clk, Log: log,
		Writer: &stats.Writer{Vol: stats.VFS{FS: fs, St: node.FSM, Rd: vfs.NewReader(node.FSM, prober, client, nil, nil)}, Name: cfg.Node.Name, Spill: filepath.Join(cfg.Node.DataDir, "stats", "pending"), Log: log},
		Src: stats.Sources{
			Machine: func() *admin.MachineReport { return admin.Vitals(cfg.Node.DataDir, ident.IP) },
			Gateway: func() *admin.GatewayReport {
				return admin.GatewayFromDirty(ov.Dirty()).WithFlusher(fl, ov).WithReader(rd).WithFS(fs)
			},
			Process:  admin.Process,
			IsLeader: node.IsLeader,
			Term:     node.Raft.CurrentTerm,
			Health:   func() admin.VolumeHealth { return admin.ScanHealth(node.FSM, prober, rcfg.FillAfter) },
			Stats:    node.FSM.Stats,
			Members:  node.FSM.Nodes,
			Live:     func(idx meta.NodeIdx) int { return stats.LiveState(prober.IsLive(idx), prober.IsSuspect(idx)) },
			Moved: func() (int64, bool) {
				p, ok := repairer.Progress()
				return p.BytesMoved, ok
			},
		},
	}
	// the index in the volume (docs/18): the leader's pass, every node's queries
	sset, err := cfg.SearchSettings()
	if err != nil {
		return err
	}
	searchSvc := &search.Service{
		Src: node.FSM, Vol: search.VFS{FS: fs, St: node.FSM, Rd: vfs.NewReader(node.FSM, prober, client, nil, nil)}, Clk: clk, Log: log,
		Cfg: search.Config{MaxFile: sset.MaxFile, Workers: sset.Workers, Batch: sset.Batch, BatchBytes: sset.BatchBytes, MaxBytes: sset.MaxBytes},
	}
	ops := &adminOps{node: node, prober: prober, repairer: repairer, gc: gc, disks: disks, client: client, rd: rd, ring: ring, fs: fs, pins: pinned, stats: statsSvc, search: searchSvc, self: selfIdx, clk: clk, log: log, dataDir: cfg.Node.DataDir}
	// this host's mirrors (docs/16): the hook takes the clone of a snapshot begun on one
	mpol, err := cfg.Mirroring()
	if err != nil {
		return err
	}
	mlimit := repair.NewLimiter(mpol.Rate, clk)
	mirrors := newMirrorSet(sysfs.OS{}, cfg, mpol, clusterID, node.FSM, clk, log)
	mirrors.self, mirrors.propose = node.SelfIdx, ops.propose
	mirrors.read = func(ctx context.Context, h meta.Hash) ([]byte, error) {
		b, err := rd.ReadChunk(chunk.Background(ctx), h) // at background priority: client reads keep their half (I-175)
		mlimit.Wait(ctx, int64(len(b)))
		return b, err
	}
	node.FSM.SetSnapshotHook(mirrors.onBegin)
	defer node.FSM.SetSnapshotHook(nil)
	ops.mirrors, ops.mirrorPol = mirrors, mpol
	fs.SetMirrors(mirrorSource{local: mirrors, client: client, st: node.FSM, live: prober.IsLive}) // /.snapshots (docs/16 §6)
	webAddr := ""                                                                                  // the browser UI listener (docs/13); listen.web 0 turns it off
	if cfg.Listen.Web != 0 {
		webAddr = net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Web))
	}
	// the report walks every chunk, so a refresher builds it off the request path and a
	// request serves the latest build (docs/12 §8, I-205)
	reporter := admin.NewReporter(func() admin.Report {
		start := time.Now()
		rep := admin.Build(node, prober, clusterID, clk, repairer)
		rep.WalkMS = time.Since(start).Milliseconds()
		rep.Gateway = admin.GatewayFromDirty(ov.Dirty()).WithFlusher(fl, ov).WithReader(rd).WithFS(fs)
		rep.Machine = admin.Vitals(cfg.Node.DataDir, ident.IP)
		if w := admin.LowDiskWarning(rep.Machine); w != "" {
			rep.Warnings = append(rep.Warnings, w)
		}
		cacheRep := pinned.List()
		rep.Cache = &cacheRep
		var mwarn []string
		rep.Mirrors, mwarn = mirrors.report(clk.Now(), node.FSM.Snapshots())
		rep.Warnings = append(rep.Warnings, mwarn...)
		searchRep := searchSvc.Status()
		rep.Search = &searchRep
		if webAddr != "" {
			rep.WebURL = "http://" + webAddr + "/"
		}
		return rep
	}, clk)
	defer reporter.Close()
	status := func(fresh bool) any { return reporter.Get(fresh) }
	srv, err := rpc.NewServer(rpc.Options{
		NodeID: id, ClusterID: clusterID, Version: version, Disk: disks.lookup, DiskHealth: disks.health, Mirror: mirrors.get, Applier: node, Fence: fence{node},
		Joiner: node, Admin: ops, Auth: auth, Members: node.FSM.Nodes, JoinToken: func() string { return node.FSM.Meta().JoinToken },
		Status: status, Clk: clk, Logger: log, MaxShard: int64(vol.ChunkSize) + 4096,
		PutSlots: rpc.Slots(int64(vol.ChunkSize) + 4096), GetSlots: rpc.Slots(int64(vol.ChunkSize) + 4096),
		Grace: rcfg.Grace, BadSlot: repairer.ReportBad, Dev: devHandler, Blocked: blocked,
		Stats: func(since int64) any { return statsSvc.Live(since) }, Web: webPort(cfg),
	})
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
		cacheBytes, err := cfg.StatsCacheBytes()
		if err != nil {
			return err
		}
		query := &stats.Query{
			Vol: stats.VFS{FS: fs, St: node.FSM, Rd: vfs.NewReader(node.FSM, prober, client, nil, nil)}, Clk: clk, Keep: statsSvc.Keep, CacheBytes: cacheBytes, Log: log,
			Self: statsSvc.Live, Fetch: stats.FetchRPC(client),
			Members: func() []stats.Member {
				probes := prober.Snapshot()
				var out []stats.Member
				all := node.FSM.Nodes()
				for i := range all {
					n := &all[i]
					if n.Kind != meta.KindDevice || n.State == meta.Removed {
						continue
					}
					m := stats.Member{Name: n.Name, Addr: net.JoinHostPort(n.Addr, strconv.Itoa(int(n.RPCPort))), Live: prober.IsLive(n.Idx), Self: n.Idx == selfIdx, Web: probes[n.Idx].Web}
					if m.Self {
						m.Web = webPort(cfg)
					}
					out = append(out, m)
				}
				return out
			},
		}
		web, err = webui.New(webui.Options{
			FS: fs, Store: node.FSM, Auth: auth, Login: login, Status: func() any { return status(false) }, Stats: query, Search: searchSvc, Catalog: searchSvc, Hosts: webui.Hosts(webAddr, ident.DNSName),
			Chunks: func(ctx context.Context, sel string, offset, limit int) (int, []admin.ChunkReport, error) {
				return admin.DescribeChunksPage(ctx, node.FSM, prober, client, ring, sel, offset, limit)
			}, Clk: clk, Logger: log, Retention: rcfg.Retention, Pins: pinned,
		})
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
	rl, err := rpc.Listen(ctx, rpcAddr, auth)
	if err != nil {
		return err
	}
	// the gateways: NFS on macOS and Linux, SMB on Windows, either or both by config
	// (docs/07, docs/15); a listener that fails to bind closes the ones before it
	open := []net.Listener{rl}
	closeOpen := func() {
		for _, l := range open {
			_ = l.Close()
		}
	}
	var nl, sl, wl net.Listener
	var smbSrv *smb.Server
	if cfg.Listen.NFS != "" {
		if nl, err = nfsgw.Listen(ctx, cfg.Listen.NFS); err != nil {
			closeOpen()
			return err
		}
		open = append(open, nl)
		nfsgw.UseLogger(log) // one log stream, one format (the library logs client noise)
	}
	if cfg.Listen.SMB != "" {
		if smbSrv, err = smbgw.NewServer(fs, node.FSM, cfg.SMB.Password); err != nil {
			closeOpen()
			return err
		}
		if sl, err = smbgw.Listen(ctx, cfg.Listen.SMB); err != nil {
			closeOpen()
			return err
		}
		open = append(open, sl)
	}
	if web != nil {
		var lc net.ListenConfig
		if wl, err = lc.Listen(ctx, "tcp", webAddr); err != nil {
			closeOpen()
			return fmt.Errorf("web ui listener: %w", err)
		}
	}
	log.Info("tailraid serving", "version", version, "node", id, "idx", selfIdx, "tailnet", ident.IP, "rpc", rl.Addr(), "raft", raftAddr,
		"nfs", addrOrOff(nl), "smb", addrOrOff(sl), "data", cfg.Node.DataDir, "policy", fmt.Sprintf("%d+%d", vol.K, vol.M))
	if nl != nil && runtime.GOOS != "windows" {
		prog, args, _ := mount.Command(runtime.GOOS, nl.Addr().String(), cfg.Mount.Path)
		log.Info("mount with", "cmd", prog+" "+strings.Join(args, " "))
	}
	if sl != nil { // the password is read from stdin by `net use … *`, so the line holds none
		prog, args, _ := mount.SMBCommand(runtime.GOOS, sl.Addr().String(), cfg.Mount.Path)
		log.Info("mount with", "cmd", prog+" "+strings.Join(args, " "))
	}
	if web != nil {
		log.Info("web ui", "url", "http://"+wl.Addr().String()+"/", "users", webUsers)
	}

	// background loops: RPC server, prober, address watch, flusher, repairer, maintenance, pins, stats, search, web UI
	bg, cancelBg := context.WithCancel(context.Background())
	loops := 10 // mirrors.worker included (docs/16 §3)
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
	vf := &voterFile{cfg: cfg, voter: cfg.Node.Voter}
	go func() {
		if watchSelf(bg, ts, node, client, id, selfIdx, ident.IP, version, vf, log) {
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
	go func() {
		mirrors.worker(bg, func(ctx context.Context) error { return node.WaitReady(ctx, time.Hour) })
		done <- struct{}{}
	}()
	go func() { pinned.Run(bg); done <- struct{}{} }()
	go func() { statsSvc.Run(bg); done <- struct{}{} }()
	go func() { ops.searchLoop(bg, sset.Interval); done <- struct{}{} }()
	if web != nil {
		go func() { _ = web.Serve(bg, wl); done <- struct{}{} }()
	}

	// the gateways own the process lifetime: the first to end ends the node (the service
	// manager restarts it), and the other, if any, is stopped with the serve context
	gw := make(chan error, 2)
	gateways := 0
	if nl != nil {
		gateways++
		go func() { gw <- nfsgw.Serve(sctx, nl, nfsgw.New(fs, node.FSM)) }()
	}
	if sl != nil {
		gateways++
		go func() { gw <- smbgw.Serve(sctx, sl, smbSrv) }()
	}
	select {
	case err = <-gw:
	case err = <-fatal:
	}
	stop()
	for range gateways - 1 {
		<-gw
	}
	ops.sealStats() // the partial hour, before the flusher drains and the leader hands over (I-192)
	cancelBg()
	for range loops {
		<-done
	}
	if addrChanged.Load() {
		return ErrAddressChanged
	}
	return err
}

// addrOrOff is a gateway listener's address for the log, "off" for a gateway not configured.
func addrOrOff(l net.Listener) string {
	if l == nil {
		return "off"
	}
	return l.Addr().String()
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

// staleness is the gateway's stale predicate (docs/02 §Health): the Stale state, or no
// leader at all for the threshold. A short election is not stale, so metadata mutations
// wait for it instead of failing. Raft dates the last leader contact only for a
// follower; a node that was the leader until a moment ago has none, so for it the outage
// is dated from when the gateway first saw no leader (the three-VM rounds caught a
// gateway refusing a create during a two-second election for that reason).
type staleness struct {
	mu            sync.Mutex
	noLeaderSince time.Time
}

func (s *staleness) stale(state cluster.HealthState, lastContact, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case cluster.Stale:
		return true
	case cluster.NoLeader:
		if s.noLeaderSince.IsZero() {
			s.noLeaderSince = now
		}
		if !lastContact.IsZero() && now.Sub(lastContact) >= cluster.StaleThreshold {
			return true
		}
		return now.Sub(s.noLeaderSince) >= cluster.StaleThreshold
	default:
		s.noLeaderSince = time.Time{}
		return false
	}
}

// ErrAddressChanged is returned by Serve after the node's Tailscale address changed and
// was re-registered: the listeners are bound to the old address, so the process exits for
// the service manager to restart it (docs/05, L-99).
var ErrAddressChanged = errors.New("node: tailscale address changed and re-registered; restart to bind the new address")

// adminOps implements rpc.Admin and runs the periodic GC and sweep.
type adminOps struct {
	node      *cluster.Node
	prober    *cluster.Prober
	repairer  *repair.Repairer
	gc        *repair.GC
	disks     *diskSet
	client    *rpc.Client
	rd        *vfs.Reader
	ring      *meta.EventRing
	fs        *vfs.FS
	pins      *pins.Pins
	stats     *stats.Service
	search    *search.Service
	self      meta.NodeIdx
	clk       clock.Clock
	log       *slog.Logger
	dataDir   string
	seedKey   string     // the device members the seed file holds (refreshSeed)
	s3Swept   time.Time  // the leader's last S3 sweep (docs/06 §3)
	mirrors   *mirrorSet // this host's mirrors (docs/16)
	mirrorPol config.MirrorPolicy
}

// ErrLegacyState is a restored state from before devices stopped being targets
// (docs/14 §9, I-119): nothing migrates it, so serve refuses to run on it.
var ErrLegacyState = errors.New("node: this cluster stores shards on device rows")

// legacyCheck refuses a state in which a device row carries a budget or a chunk slot
// names a device index, with the way out.
func legacyCheck(st *meta.Store) error {
	devices, slots := meta.LegacyDeviceTargets(st.Nodes(), st.ForEachChunk)
	if len(devices) == 0 && slots == 0 {
		return nil
	}
	names := make([]string, 0, len(devices))
	for i := range devices {
		names = append(names, devices[i].Name)
	}
	return fmt.Errorf("%w (%d device rows with a budget: %s; %d chunk slots on devices): this cluster was created by a version that stored shards in the data directory, "+
		"which this version cannot serve. Re-create it: `tailraid reset --yes` on every device, then `tailraid init` on one, `tailraid join` on the others (with disks listed under node.disks), and copy the data back",
		ErrLegacyState, len(devices), strings.Join(names, ", "), slots)
}

// openDisk opens a just-added disk. The CLI registered it through the leader a moment
// ago, and this node may not have applied that entry yet: the marker check then finds
// no row, so it retries for a few seconds rather than leaving the disk closed until the
// next maintenance tick.
func (a *adminOps) openDisk(ctx context.Context, path string) string {
	deadline := a.clk.Now().Add(openDiskWait)
	for {
		a.disks.refresh(a.node.FSM.Nodes())
		state := a.disks.state(path)
		if state == "open" || !a.clk.Now().Before(deadline) {
			return state
		}
		t := a.clk.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return state
		case <-t.C():
		}
	}
}

// openDiskWait bounds how long open_disk waits for the disk's row to reach this node.
const openDiskWait = 10 * time.Second

// targets lists this host's shard stores: its open disks (none on a diskless host).
func (a *adminOps) targets() []target {
	open := a.disks.open()
	out := make([]target, 0, len(open))
	for _, ld := range open {
		out = append(out, target{idx: ld.idx, store: ld.store, pinned: ld.cfg.Capacity != ""})
	}
	return out
}

// rebudget opens any disk that has come up, re-derives every unpinned target's budget
// from its filesystem (shared filesystems divided, I-91) and registers each through the
// leader when it moved past the hysteresis (docs/01 §Node budget, I-74).
func (a *adminOps) rebudget(ctx context.Context) {
	a.disks.refresh(a.node.FSM.Nodes())
	bs, err := budgets(a.targets())
	if err != nil {
		a.log.Warn("budget: measuring a disk failed", "err", err)
		return
	}
	// a pinned budget is exact: register it whenever the cluster holds another value, so a
	// disk grown in place (its capacity raised, then a restart) is seen at once
	pinned := map[meta.NodeIdx]bool{}
	for _, t := range a.targets() {
		if t.pinned {
			bs[t.idx], pinned[t.idx] = uint64(max(t.store.Capacity(), 0)), true
		}
	}
	for idx, b := range bs {
		nd, ok := a.node.FSM.Node(idx)
		if !ok || nd.Capacity == b || (!pinned[idx] && !chunk.BudgetChanged(nd.Capacity, b)) {
			continue
		}
		op := rpc.MemberOp{Op: "set_capacity", NodeIdx: idx, Capacity: b}
		if a.node.IsLeader() {
			err = a.node.MemberOp(ctx, op)
		} else if addr := a.node.LeaderRPCAddr(); addr != "" {
			err = a.client.Members(ctx, addr, op)
		} else {
			err = cluster.ErrNoLeader
		}
		if err != nil {
			a.log.Warn("budget: update failed, will retry", "target", nd.Name, "err", err)
			continue
		}
		a.log.Info("budget registered", "target", nd.Name, "was", nd.Capacity, "now", b, "pinned", pinned[idx])
	}
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
	for i := range cmds {
		res, index, err := a.node.Propose(ctx, cmds[i])
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
// refreshSeed rewrites <data_dir>/members.seed when the device members changed, so the
// next start accepts the leader before the log arrives (I-92).
func (a *adminOps) refreshSeed() {
	nodes := a.node.FSM.Nodes()
	var key strings.Builder
	for i := range nodes {
		if n := &nodes[i]; n.Kind == meta.KindDevice {
			fmt.Fprintf(&key, "%s|%s|%s|%d;", n.ID, n.Addr, n.TSStableID, n.State)
		}
	}
	if key.String() == a.seedKey {
		return
	}
	if err := cluster.WriteMemberSeed(a.dataDir, nodes); err != nil {
		a.log.Warn("member seed not written", "err", err)
		return
	}
	a.seedKey = key.String()
}

// searchLoop runs the leader's index pass every interval while the FSM moved, every
// Backlog while candidates are pending; a follower keeps its chain loaded (docs/18 §5).
// An interval of 0 turns the pass off.
func (a *adminOps) searchLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		<-ctx.Done()
		return
	}
	t := a.clk.NewTimer(every)
	defer t.Stop()
	var last uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
		}
		next := every
		applied := a.node.FSM.Applied()
		switch {
		case !a.node.IsLeader() || !a.healthy():
			a.search.Warm(ctx)
		case applied != last:
			res, err := a.search.Pass(ctx)
			switch {
			case err != nil:
				a.log.Warn("search pass failed", "err", err)
			case res.Pending > 0:
				next = min(every, search.Backlog)
			default:
				last = applied
			}
			if err == nil && res.Indexed > 0 {
				a.log.Info("search pass", "indexed", res.Indexed, "pending", res.Pending, "segment", res.Seq, "rebuilt", res.Rebuilt)
			}
		}
		t.Reset(next)
	}
}

func (a *adminOps) maintenance(ctx context.Context, cfg repair.Config) {
	gcT := a.clk.NewTimer(cfg.GCInterval)
	sweepT := a.clk.NewTimer(time.Hour)
	budgetT := a.clk.NewTimer(cfg.ScanInterval) // once a minute (docs/01 §Node budget)
	defer gcT.Stop()
	defer sweepT.Stop()
	defer budgetT.Stop()
	// a budget changed while the service was down (`volume grow` pins a disk and restarts
	// it) is registered now, not a tick later: status reads the host's free space at once,
	// and the node table must not lag it (I-74)
	a.rebudget(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-budgetT.C():
			a.rebudget(ctx) // derived budgets past the hysteresis, pinned ones whenever they differ
			a.refreshSeed()
			if a.mirrors != nil && a.healthy() {
				a.mirrors.tick(ctx) // this host's mirrors: schedule, thinning, reconcile (docs/16 §4, §5)
				if a.node.IsLeader() {
					if n := expireSyncs(ctx, a.node.FSM.Snapshots(), a.clk.Now(), a.mirrorPol.HoldMax, a.propose); n > 0 {
						a.log.Warn("snapshot syncs ended past hold_max", "count", n)
					}
				}
			}
			budgetT.Reset(cfg.ScanInterval)
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
			for _, t := range a.targets() { // open targets only: a closed disk is never swept (I-86)
				if n, err := repair.SweepLocal(ctx, t.store, a.node.FSM, t.idx, cfg.Grace, a.clk, a.healthy, a.node.AppliedIndex, a.leaderApplied); err != nil {
					a.log.Warn("sweep failed", "target", t.idx, "err", err)
				} else if n > 0 {
					a.log.Info("sweep", "target", t.idx, "orphans_removed", n)
				}
			}
			a.sweepS3(ctx, cfg.Grace)
			if n, err := webui.SweepUploads(ctx, a.fs, a.node.FSM, a.clk.Now(), webui.UploadTTL); err != nil {
				a.log.Warn("upload sweep failed", "err", err)
			} else if n > 0 {
				a.log.Info("sweep", "stale_uploads_removed", n)
			}
			if n, err := webui.SweepThumbs(ctx, a.fs, a.node.FSM); err != nil {
				a.log.Warn("thumbnail sweep failed", "err", err)
			} else if n > 0 {
				a.log.Info("sweep", "orphan_thumbnails_removed", n)
			}
			if a.search != nil && a.node.IsLeader() && a.healthy() { // the index's old segments (docs/18 §4, I-206)
				if n, err := a.search.Sweep(ctx); err != nil {
					a.log.Warn("search sweep failed", "err", err)
				} else if n > 0 {
					a.log.Info("sweep", "index_segments_removed", n)
				}
			}
			if a.stats != nil && a.node.IsLeader() && a.healthy() { // the stats retention is the leader's (I-194)
				if n, err := a.stats.Retain(ctx); err != nil {
					a.log.Warn("stats retention failed", "err", err)
				} else if n > 0 {
					a.log.Info("stats retention", "segments_removed", n)
				}
			}
			sweepT.Reset(time.Hour)
		}
	}
}

// S3SweepInterval is how often the leader sweeps each bucket: every LIST page is a billed
// request, and an orphan object costs only its storage meanwhile (docs/06 §3).
const S3SweepInterval = 24 * time.Hour

// sweepS3 runs the leader's daily sweep of every S3 node that is not removed (I-138).
func (a *adminOps) sweepS3(ctx context.Context, grace time.Duration) {
	if !a.node.IsLeader() || !a.healthy() || (!a.s3Swept.IsZero() && a.clk.Now().Sub(a.s3Swept) < S3SweepInterval) {
		return
	}
	a.s3Swept = a.clk.Now()
	nodes := a.node.FSM.Nodes()
	for i := range nodes {
		nd := &nodes[i]
		if nd.Kind != meta.KindS3 || nd.State == meta.Removed {
			continue
		}
		if n, err := repair.SweepS3(ctx, a.client, a.node.FSM, nd.Idx, grace, a.clk.Now()); err != nil {
			a.log.Warn("s3 sweep failed", "node", nd.Name, "err", err)
		} else if n > 0 {
			a.log.Info("s3 sweep", "node", nd.Name, "orphans_removed", n)
		}
	}
}

func (a *adminOps) AdminOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	switch op.Op {
	case "repair_now", "check", "set_policy", "gc_now", "rotate_token", "trash_restore", "trash_purge", "index_now":
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
	case "trash_ls", "trash_restore", "trash_purge":
		return a.trashOp(ctx, op)
	case "cache_pin": // this node's pins (docs/12 §10): never forwarded
		return a.pins.Pin(op.Path)
	case "cache_unpin":
		if err := a.pins.Unpin(op.Path); err != nil {
			return nil, err
		}
		return map[string]string{"path": op.Path}, nil
	case "cache_ls":
		return a.pins.List(), nil
	case "repair_now":
		return a.repairer.Scan(ctx)
	case "gc_now":
		return a.gc.RunOnce(ctx)
	case "index_now": // the leader's pass now (docs/18 §5, a pass op, I-189)
		return a.search.Pass(ctx)
	case "search": // this node's index (docs/18 §6): never forwarded
		return a.search.Search(ctx, op.Query, op.Path, op.Limit), nil
	case "stats_seal": // this node's partial stats hour and any rollup still missing (docs/17 §7)
		if a.stats == nil {
			return nil, errors.New("stats: no sampler")
		}
		n := a.stats.Writer.Unsealed()
		if err := a.stats.Seal(ctx, op.Rollup); err != nil {
			return nil, err
		}
		return map[string]any{"sealed_rows": n}, nil
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
	case "snapshot_ls": // the catalogue (docs/16 §5), every mirror online or not
		return snapshotList(a.node.FSM.Nodes(), a.node.FSM.Snapshots(), op.Mirror), nil
	case "snapshot_create": // manual snapshots: permanent, never thinned (docs/16 §4)
		return a.snapshotCreate(ctx, op)
	case "versions", "versions_revert", "versions_copy", "snapshot_restore": // on the node asked (docs/12 §11)
		return a.restoreOp(ctx, op)
	case "export", "snapshot": // docs/12 §9: the newest state, exported under the data directory for the CLI to copy out ("snapshot" is the name before docs/16)
		dir, m, err := a.node.ExportSnapshot(filepath.Join(a.dataDir, "export"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"dir": dir, "index": m.Index, "term": m.Term}, nil
	case "open_disk":
		if !filepath.IsAbs(op.Path) {
			return nil, fmt.Errorf("open_disk: %q is not an absolute path", op.Path)
		}
		a.disks.add(config.Disk{Path: op.Path, Capacity: op.Capacity})
		return map[string]string{"state": a.openDisk(ctx, op.Path)}, nil
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
		var prog *repair.Progress
		if p, running := a.repairer.Progress(); running {
			prog = &p
		}
		rep := admin.Check(a.node.FSM, a.prober, a.repairer.Config().FillAfter, st, prog, op.Files)
		_, rep.Warnings = a.mirrors.report(a.clk.Now(), a.node.FSM.Snapshots())
		return rep, nil
	case "fsck":
		snap, err := a.node.FSM.Snapshot()
		if err != nil {
			return nil, err
		}
		fs := snap.(interface{ State() *meta.State })
		rep := admin.FsckReport{Violations: meta.CheckInvariants(fs.State())}
		for _, t := range a.targets() {
			_ = t.store.Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error {
				h, kk, mm, idx, err := chunk.ParseKey(k)
				if err != nil {
					rep.OrphanShards++ // a file in the shard tree that is not a shard: junk the sweep will not touch
					return nil         //nolint:nilerr // counted above; the walk goes on
				}
				c, ok := a.node.FSM.GetChunk(h)
				if !ok || c.K != kk || c.M != mm || idx >= len(c.Placement) || c.Placement[idx] != t.idx {
					rep.OrphanShards++
				}
				return nil
			})
		}
		rep.OK = len(rep.Violations) == 0
		return rep, nil
	}
	return nil, fmt.Errorf("unknown admin op %q", op.Op)
}

// filesReferencing walks the tree once and returns the paths whose chunks are in set.

// trashOp serves `tailraid trash ls|restore|purge` (docs/12 §10): a listing from this
// node's FSM copy, a restore or a purge proposed on the leader.
func (a *adminOps) trashOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	st := a.node.FSM
	now := a.clk.Now().UnixNano()
	switch op.Op {
	case "trash_ls":
		var dir *meta.Ino
		if op.Path != "" {
			in, err := st.Resolve(meta.SplitPath(op.Path))
			if err != nil {
				return nil, err
			}
			if in.Type != meta.TDir {
				return nil, meta.ErrNotDir
			}
			dir = &in.Ino
		}
		return map[string]any{"entries": admin.TrashList(st, dir, a.gc.Retention)}, nil
	case "trash_restore":
		c := &meta.Restore{Ino: meta.Ino(op.Ino), NewName: op.As, Now: now}
		if op.To != "" {
			in, err := st.Resolve(meta.SplitPath(op.To))
			if err != nil {
				return nil, err
			}
			c.NewParent = in.Ino
		}
		if err := a.propose(ctx, meta.Command{Op: meta.OpRestore, Restore: c}); err != nil {
			return nil, err
		}
		parts, err := st.PathOf(c.Ino)
		if err != nil {
			return nil, err
		}
		return map[string]string{"path": "/" + strings.Join(parts, "/")}, nil
	default: // trash_purge
		if op.Ino != 0 {
			was := st.Stats().TrashInodes
			if err := a.propose(ctx, meta.Command{Op: meta.OpPurge, Purge: &meta.Purge{Ino: meta.Ino(op.Ino), Now: now}}); err != nil {
				return nil, err
			}
			return map[string]int{"purged": was - st.Stats().TrashInodes}, nil
		}
		before := max(now, st.LastNow()) + 1 // --all: everything, however young
		if op.OlderThan != "" {
			d, err := time.ParseDuration(op.OlderThan)
			if err != nil || d < 0 {
				return nil, fmt.Errorf("trash purge: older-than %q: want a duration", op.OlderThan)
			}
			if d > 0 {
				before = max(now, st.LastNow()) - d.Nanoseconds()
			}
		} else if !op.All {
			return nil, errors.New("trash purge: one of --ino, --older-than or --all is required")
		}
		n, err := a.gc.PurgeTrash(ctx, before)
		if err != nil {
			return nil, err
		}
		return map[string]int{"purged": n}, nil
	}
}

// propose applies one command on the leader and waits for it locally.
func (a *adminOps) propose(ctx context.Context, cmd meta.Command) error {
	res, index, err := a.node.Propose(ctx, cmd)
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return err
	}
	return a.node.FSM.WaitApplied(ctx, index)
}

// fence answers the DELETE fence from the local FSM for one of this host's disks
// (rpc.Fence).
type fence struct{ n *cluster.Node }

func (f fence) PlacedOn(target meta.NodeIdx, key chunk.ShardKey) bool {
	h, k, m, idx, err := chunk.ParseKey(key)
	if err != nil {
		return false
	}
	c, ok := f.n.FSM.GetChunk(h)
	return ok && c.K == k && c.M == m && idx < len(c.Placement) && c.Placement[idx] == target
}

// devStores is /dev/*'s view of this host's open disks (docs/12 §3).
type devStores struct {
	disks *diskSet
	fsm   *meta.Store
}

// Paths is every copy of k on this host's open disks (a rebalance within the host may
// have left one behind the placed copy until the sweep).
func (d devStores) Paths(k chunk.ShardKey) []string {
	var out []string
	for _, ld := range d.disks.open() {
		if ld.store.Has(k) {
			out = append(out, ld.store.Path(k))
		}
	}
	return out
}

// Disks is GET /dev/disk: every open disk as its budget sees it.
func (d devStores) Disks() []dev.DiskInfo {
	var out []dev.DiskInfo
	for _, ld := range d.disks.open() {
		used, free, total, err := ld.store.Disk()
		if err != nil {
			continue
		}
		info := dev.DiskInfo{ID: ld.id, Used: used, Free: free, Total: total, Pinned: ld.cfg.Capacity != ""}
		if row, ok := d.fsm.Node(ld.idx); ok {
			info.Name = row.Name
		}
		out = append(out, info)
	}
	return out
}

// Fill is POST /dev/fill: the scratch file on the named disk ("" is the first open one).
func (d devStores) Fill(id string, n uint64) error {
	for _, ld := range d.disks.open() {
		if id == "" || ld.id == id {
			return dev.Fill(ld.cfg.Path, n)
		}
	}
	return fmt.Errorf("no open disk %q on this host", id)
}

// watchSelf keeps this node's row current. At start it reports the version this binary
// runs when the row names another (retrying until a leader accepts it, I-133), and it
// re-registers the node when its Tailscale IP changes (docs/02 §IP change), returning
// true once the new address is in the node table so the caller can restart.
func watchSelf(ctx context.Context, ts tailscale.Client, node *cluster.Node, client *rpc.Client, id string, self meta.NodeIdx, current netip.Addr, version string, vf *voterFile, log *slog.Logger) bool {
	t := time.NewTicker(cluster.ProbeInterval)
	defer t.Stop()
	reported := false
	for {
		vf.follow(node, id, log) // the file's vote follows the row (I-176)
		if !reported {
			var err error
			if reported, err = reportVersion(ctx, node, client, id, self, version); err != nil {
				log.Warn("version report failed, will retry", "err", err)
			} else if reported {
				log.Info("version reported to the node table", "version", version)
			}
		}
		if st, err := ts.Status(ctx); err == nil {
			if ip, err := tailscale.SelfIPv4(st); err == nil && ip != current {
				log.Warn("tailscale address changed", "old", current, "new", ip)
				if err := selfMemberOp(ctx, node, client, rpc.MemberOp{Op: "update_addr", NodeIdx: self, Addr: ip.String()}); err != nil {
					log.Warn("address update failed, will retry", "err", err)
				} else {
					log.Warn("address re-registered; exiting so the service manager restarts the listeners on it", "addr", ip)
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// voterFile keeps the config file's `node.voter` equal to this node's row (docs/12 §7,
// I-176): the row is the truth (`node promote|demote` change it on the leader, from any
// device), the key is what init or join asked for and what an operator reads back. The
// file is rewritten only when the two differ; one that cannot be written is warned
// about, at most once a minute, and serving goes on.
type voterFile struct {
	cfg   config.Config // as loaded; Path names the file ("" when there is none)
	voter bool          // the value the file holds
	warn  logx.Limiter
}

func (v *voterFile) follow(node *cluster.Node, id string, log *slog.Logger) {
	row, ok := node.FSM.NodeByID(id)
	if !ok || v.cfg.Path == "" || row.Voter == v.voter {
		return
	}
	v.cfg.Node.Voter = row.Voter
	if err := config.Save(v.cfg.Path, v.cfg); err != nil {
		v.warn.Warn(log, "config not updated to the member table's vote", "path", v.cfg.Path, "voter", row.Voter, "err", err)
		return
	}
	v.voter = row.Voter
	log.Info("config updated to the member table's vote", "path", v.cfg.Path, "voter", row.Voter)
}

// reportVersion proposes set_version for this node's row when it names another version
// than the one running. It reports true once the row is current (including when it
// already was: a row that matches is never rewritten) and false with the error otherwise.
func reportVersion(ctx context.Context, node *cluster.Node, client *rpc.Client, id string, self meta.NodeIdx, version string) (bool, error) {
	cur, ok := node.FSM.NodeByID(id)
	if !ok {
		return false, meta.ErrNoSuchNode // the table has not arrived yet
	}
	if cur.Version == version {
		return true, nil
	}
	if err := selfMemberOp(ctx, node, client, rpc.MemberOp{Op: "set_version", NodeIdx: self, Version: version}); err != nil {
		return false, err
	}
	return true, nil
}

// selfMemberOp runs a member op on the leader: here when this node leads, else forwarded.
func selfMemberOp(ctx context.Context, node *cluster.Node, client *rpc.Client, op rpc.MemberOp) error {
	if node.IsLeader() {
		return node.MemberOp(ctx, op)
	}
	if addr := node.LeaderRPCAddr(); addr != "" {
		return client.Members(ctx, addr, op)
	}
	return cluster.ErrNoLeader
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
	ts, ident, err := Tailnet(ctx, cfg)
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	// a device name is looked up in the tailnet (a DNS lookup would give a LAN address
	// no node listens on); the port is this cluster's RPC port unless one was given
	st, _ := ts.Status(ctx)
	target, err := ResolveJoinTarget(st, host, cfg.Listen.RPC)
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	// learn the cluster ID from the target's health endpoint before opening the transport
	probe := rpc.NewClient([16]byte{}, nil)
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
	if err := codecCompatible(h.Codec); err != nil {
		return rpc.JoinResponse{}, err
	}
	raftAddr := net.JoinHostPort(ident.IP.String(), strconv.Itoa(cfg.Listen.Raft))
	// the transport accepts the leader's Raft connection only from a member (I-92); the
	// joiner's own table is empty until the log arrives, so the join response seeds it
	auth := &rpc.WhoIsAuthorizer{TS: ts, Clk: clock.Real()}
	// the target is the leader or will name it: its Raft connection must be accepted
	// from the first attempt, before the join response lists the members, so it is
	// seeded from WhoIs now (I-92)
	if targetHost, _, err := net.SplitHostPort(target); err == nil {
		if ip, err := netip.ParseAddr(targetHost); err == nil {
			if w, err := ts.WhoIs(ctx, ip); err == nil && w.Node.StableID != "" {
				auth.SetSeed([]meta.Node{{ID: "join-target", Kind: meta.KindDevice, Addr: ip.String(), TSStableID: w.Node.StableID, State: meta.Active}})
			}
		}
	}
	n, err := cluster.OpenFresh(cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, ClusterID: cid, RaftAddr: raftAddr, Logger: log, PeerAuth: auth})
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	defer n.Close()
	auth.SetMembers(n.FSM.NodeByAddr)
	client := rpc.NewClient(cid, nil)
	req := rpc.JoinRequest{
		Token: token, NodeID: id, Name: cfg.Node.Name, TSStableID: ident.StableID, Addr: ident.IP.String(),
		RaftPort: uint16(cfg.Listen.Raft), RPCPort: uint16(cfg.Listen.RPC), Voter: voter, Version: version,
	}
	var resp rpc.JoinResponse
	for range 5 {
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
	auth.SetSeed(resp.Members)
	// persisted for serve: if this process stops before the log has arrived, serve's
	// own member check would otherwise refuse the leader for good (I-92)
	if err := cluster.WriteMemberSeed(cfg.Node.DataDir, resp.Members); err != nil {
		return resp, err
	}
	if err := cluster.WriteClusterID(cfg.Node.DataDir, h.ClusterID); err != nil {
		return resp, err
	}
	// the disks listed in the config, registered through the leader as init registers its
	// own (docs/14 §2, I-121); serve opens them once their rows arrive
	resp.Disks, err = registerDisks(cfg, cid, id, resp.NodeIdx, func(disk meta.Node) error {
		return client.Members(ctx, target, rpc.MemberOp{Op: "add_disk", NodeIdx: resp.NodeIdx, DiskID: disk.ID, Name: disk.Name, Capacity: disk.Capacity})
	})
	if err != nil {
		return resp, err
	}
	// receive the log before stopping so `serve` starts with the node table present, and
	// snapshot it: Raft restores a table at open from a snapshot only
	if err := n.WaitReady(ctx, 60*time.Second); err != nil {
		log.Warn("joined, but the log has not fully replicated yet; serve will finish it", "err", err)
	} else if err := snapshotAfterJoin(ctx, n); err != nil {
		log.Warn("no snapshot after the join; serve relies on the member seed until the leader connects", "err", err)
	}
	return resp, nil
}

// snapshotAfterJoin takes the joiner's first snapshot. Raft refuses one until its FSM
// goroutine has passed the newest configuration entry (the joiner's own add_voter, which
// lands after the last command WaitReady waited for), and reports nothing new until the
// FSM has applied an entry past the last snapshot; on a slow machine both take longer
// than a ready serve's few seconds (CI runners did), so both are retried for up to 30 s.
func snapshotAfterJoin(ctx context.Context, n *cluster.Node) error {
	return snapshotWithin(ctx, n, 30*time.Second, true)
}

// Snapshot takes a Raft snapshot now, retrying a refusal for a few seconds; nothing new
// to snapshot is not an error. A node restores its table at open from a snapshot only,
// so init, join, a ready serve and a clean shutdown each leave one behind (I-135).
func Snapshot(ctx context.Context, n *cluster.Node) error {
	return snapshotWithin(ctx, n, 5*time.Second, false)
}

// snapshotWithin retries a refused snapshot until bound; retryNothingNew also retries
// "nothing new to snapshot", for a node whose first entries are still being applied.
func snapshotWithin(ctx context.Context, n *cluster.Node, bound time.Duration, retryNothingNew bool) error {
	deadline := time.Now().Add(bound)
	for {
		err := n.Raft.Snapshot().Error()
		settled := err == nil || (errors.Is(err, raft.ErrNothingNewToSnapshot) && !retryNothingNew)
		if settled || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// onReady waits for the node to be ready once, then re-runs the legacy check on the
// state the log brought (a stale start checked the snapshot only) and snapshots.
func onReady(ctx context.Context, n *cluster.Node, log *slog.Logger, fatal chan<- error) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for !n.Ready() {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	// Ready proves dispatch to the FSM, not the apply: the check reads the table only
	// once the FSM holds every entry of the log (WaitReady, a barrier on the leader)
	if err := n.WaitReady(ctx, time.Minute); err != nil {
		if ctx.Err() == nil {
			log.Warn("legacy check skipped: the log did not replay", "err", err)
		}
		return
	}
	if err := legacyCheck(n.FSM); err != nil {
		fatal <- err
		return
	}
	if err := Snapshot(ctx, n); err != nil && !errors.Is(err, raft.ErrNothingNewToSnapshot) {
		log.Warn("snapshot after becoming ready failed", "err", err)
	}
}

// codecCompatible refuses to join a cluster whose node writes a newer log and snapshot
// format than this binary reads (docs/12 §6, I-105): every entry it sent would be
// undecodable here. A target that reports no codec predates the field and is older.
func codecCompatible(leader uint8) error {
	if leader > meta.CodecVersion {
		return fmt.Errorf("the cluster writes codec version %d and this binary reads up to %d: update this node first (tailraid update)", leader, meta.CodecVersion)
	}
	return nil
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

// webPort is the browser UI's port for /health (docs/13 §1): 0 when the UI is off.
func webPort(cfg config.Config) uint16 {
	if cfg.Listen.Web <= 0 || cfg.Listen.Web > 65535 {
		return 0
	}
	return uint16(cfg.Listen.Web) //nolint:gosec // bounded above
}

// StatsSealTimeout bounds the seal a stopping node writes (docs/17 §2).
const StatsSealTimeout = 10 * time.Second

// sealStats writes the partial hour on stop; a cluster that cannot take it (no leader, a
// stale gateway) gets the rows spilled locally for the next start.
func (a *adminOps) sealStats() {
	if a.stats == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), StatsSealTimeout)
	defer cancel()
	if err := a.stats.Seal(ctx, false); err != nil {
		a.log.Warn("stats: the stop's seal did not reach the volume", "err", err)
	}
}
