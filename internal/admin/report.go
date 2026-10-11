// Package admin builds the reports behind `status`, `node`, `volume` and `/v1/status`.
package admin

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/vfs"
)

// NodeReport is one row of `node ls`.
type NodeReport struct {
	Idx        meta.NodeIdx `json:"idx"`
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Kind       string       `json:"kind"`
	Host       meta.NodeIdx `json:"host,omitempty"` // disks: the device that serves it (docs/14)
	State      string       `json:"state"`
	Addr       string       `json:"addr"`
	RPCPort    uint16       `json:"rpc_port"`
	Voter      bool         `json:"voter"`
	Leader     bool         `json:"leader"`
	Self       bool         `json:"self"`
	Live       bool         `json:"live"`
	Suspect    bool         `json:"suspect,omitempty"` // a shard RPC to it failed; excluded until its next good probe (docs/05 §6)
	DeadFor    string       `json:"dead_for,omitempty"`
	Lost       bool         `json:"lost,omitempty"` // dead longer than evict_after (MemberLost): remove it
	RTT        string       `json:"rtt,omitempty"`
	Free       int64        `json:"free"`
	Capacity   uint64       `json:"capacity"`
	Version    string       `json:"version"`
	Applied    uint64       `json:"applied"`
	Slots      int          `json:"slots"`
	Missing    int          `json:"missing_slots"` // slots this node should hold but is dead for
	Dev        bool         `json:"dev,omitempty"` // /dev/* endpoints enabled
	TSStable   string       `json:"ts_stable_id,omitempty"`
	JoinedAt   time.Time    `json:"joined_at"`
	Diskless   bool         `json:"diskless,omitempty"`    // a device with no disk rows: a gateway-only host (docs/14 §9)
	SharedWith []string     `json:"shared_with,omitempty"` // targets of the same host on the same physical disk
	S3         *S3Report    `json:"s3,omitempty"`          // S3 nodes: the bucket, never its credentials (I-139)
	fs         string       // filesystem ID from the last probe
}

// S3Report is the bucket of an S3 node (docs/10 §4.1).
type S3Report struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	Region    string `json:"region,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
}

// VolumeReport is `volume describe`.
type VolumeReport struct {
	K              uint8   `json:"k"`
	M              uint8   `json:"m"`
	Auto           bool    `json:"auto"` // the leader chooses K, M (docs/03)
	ChunkSize      uint32  `json:"chunk_size"`
	PolicyVersion  uint32  `json:"policy_version"`
	Inodes         int     `json:"inodes"`
	Chunks         int     `json:"chunks"`
	Tombstoned     int     `json:"tombstoned"`
	LogicalBytes   uint64  `json:"logical_bytes"`
	StoredBytes    uint64  `json:"stored_bytes"`
	DedupRatio     float64 `json:"dedup_ratio"` // logical / stored·k/(k+m)
	Capacity       uint64  `json:"capacity"`    // Σ Active capacity
	UsableCapacity uint64  `json:"usable_capacity"`
	Free           uint64  `json:"free"`
	Degraded       int     `json:"degraded_chunks"`   // chunks with < k+m good slots
	RepairPending  int     `json:"repair_pending"`    // chunks with a slot to re-place (after fill_after)
	Unreadable     int     `json:"unreadable_chunks"` // chunks with < k live slots
	StalePolicy    int     `json:"stale_policy_chunks"`
	DomainSkew     int     `json:"domain_skew"`     // chunks above the host cap, to be moved (docs/14 §7)
	DiskSkew       int     `json:"disk_skew"`       // slots the capacity rebalance would move within a host (docs/14 §7)
	ToleratesDisks int     `json:"tolerates_disks"` // targets the policy survives losing
	ToleratesHosts int     `json:"tolerates_hosts"` // whole hosts it survives losing (docs/14 §6)
}

// GatewayReport is this node's overlay: what is acknowledged but not yet committed, and
// what the flusher holds for it (docs/04 §9, docs/12 §8, I-84).
type GatewayReport struct {
	DirtyInodes  int    `json:"dirty_inodes"`
	DirtyBytes   uint64 `json:"dirty_bytes"` // bytes in extents not yet captured by a flush plan
	PlanBytes    uint64 `json:"plan_bytes"`  // bytes in flushes under way
	WALBytes     int64  `json:"wal_bytes"`   // Σ per-inode WAL file sizes
	WALCap       uint64 `json:"wal_cap"`     // the per-inode WAL cap
	Flushing     int    `json:"flushing"`    // inodes with a flush plan outstanding
	FlushBytes   uint64 `json:"flush_bytes"` // chunk and shard buffers the flusher holds right now
	FlushPeak    uint64 `json:"flush_peak"`  // the most it has held since start
	FlushCap     uint64 `json:"flush_cap"`   // the bound of docs/04 §9 at the current policy
	LastFlushErr string `json:"last_flush_err,omitempty"`
	// readahead (docs/04 §5, I-166): chunks a prefetch fetched, READs served by one, and
	// the prefetches in flight right now
	ReadaheadFetched uint64 `json:"readahead_fetched"`
	ReadaheadJoined  uint64 `json:"readahead_joined"`
	Prefetching      int    `json:"prefetching"`
}

// WithReader adds the reader's readahead counters to the gateway view.
func (g *GatewayReport) WithReader(rd *vfs.Reader) *GatewayReport {
	g.ReadaheadFetched, g.ReadaheadJoined = rd.ReadaheadStats()
	g.Prefetching = rd.Prefetching()
	return g
}

// GatewayFromDirty folds the overlay's dirty list into the status view.
func GatewayFromDirty(infos []vfs.DirtyInfo) *GatewayReport {
	g := &GatewayReport{DirtyInodes: len(infos)}
	for i := range infos {
		g.DirtyBytes += infos[i].DirtyBytes
		g.PlanBytes += infos[i].PlanBytes
		g.WALBytes += infos[i].WALBytes
		if infos[i].Flushing {
			g.Flushing++
		}
		if infos[i].LastErr != "" && g.LastFlushErr == "" {
			g.LastFlushErr = infos[i].LastErr
		}
	}
	return g
}

// WithFlusher adds the flusher's and the overlay's bounds to the gateway view.
func (g *GatewayReport) WithFlusher(fl *vfs.Flusher, ov *vfs.Overlay) *GatewayReport {
	g.FlushBytes, g.FlushPeak = fl.Buffers()
	g.FlushCap = fl.BufferCap()
	g.WALCap = uint64(max(ov.WALCap(), 0))
	return g
}

// ProcessReport is the Go runtime's view of this node's process (docs/12 §8, I-84).
type ProcessReport struct {
	HeapInuse  uint64 `json:"heap_inuse"`
	Sys        uint64 `json:"sys"`
	Goroutines int    `json:"goroutines"`
}

func processReport() *ProcessReport {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &ProcessReport{HeapInuse: ms.HeapInuse, Sys: ms.Sys, Goroutines: runtime.NumGoroutine()}
}

// Report is the whole `status` document.
type Report struct {
	Node        string         `json:"node"`
	ClusterID   string         `json:"cluster_id"`
	Health      string         `json:"health"`
	Ready       bool           `json:"ready"` // this node knows a leader and has applied every committed entry (I-125)
	Leader      string         `json:"leader"`
	LastContact string         `json:"last_contact,omitempty"`
	Voters      int            `json:"voters"`
	LiveVoters  int            `json:"live_voters"`
	Warnings    []string       `json:"warnings,omitempty"`
	Volume      VolumeReport   `json:"volume"`
	Nodes       []NodeReport   `json:"nodes"`
	JoinToken   string         `json:"join_token,omitempty"`
	Repair      *RepairStatus  `json:"repair,omitempty"`
	Gateway     *GatewayReport `json:"gateway,omitempty"` // this node's overlay (docs/04 §8)
	Process     *ProcessReport `json:"process,omitempty"` // this node's runtime (docs/12 §8)
	// IncompatibleEntries counts log entries this binary could not decode: a newer release
	// is writing to the cluster and this node must be updated (docs/12 §6, I-105).
	IncompatibleEntries uint64         `json:"incompatible_entries,omitempty"`
	Machine             *MachineReport `json:"machine,omitempty"` // this node's OS vitals (docs/12 §8)
	WebURL              string         `json:"web_url,omitempty"` // this node's browser UI (docs/13), "" when off
	GeneratedAt         time.Time      `json:"generated_at"`
}

// RepairStatus is the leader's last scan (nil on followers).
type RepairStatus struct {
	LastScan time.Time    `json:"last_scan"`
	Stats    repair.Stats `json:"stats"`
	Reported int          `json:"reported_bad_slots"`
}

// Build assembles the report from the local FSM and prober view; rep may be nil.
func Build(n *cluster.Node, p *cluster.Prober, clusterID [16]byte, clk clock.Clock, rp *repair.Repairer) Report {
	st := n.FSM
	now := clk.Now()
	rep := Report{Node: n.ID, ClusterID: meta.HexID(clusterID), GeneratedAt: now, Process: processReport()}
	h := n.Health(clk, p)
	rep.Health, rep.Leader, rep.Voters, rep.LiveVoters = h.State.String(), h.Leader, h.Voters, h.LiveVoters
	rep.Ready = n.Ready()
	if h.LastContact > 0 {
		rep.LastContact = h.LastContact.Round(time.Millisecond).String()
	}
	rep.JoinToken = st.Meta().JoinToken
	rep.IncompatibleEntries = st.Incompatible()
	if rep.IncompatibleEntries > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d log entries from a newer release could not be decoded: update this node", rep.IncompatibleEntries))
	}

	vol := st.Volume()
	stats := st.Stats()
	rep.Volume = VolumeReport{
		K: vol.K, M: vol.M, Auto: vol.Auto, ChunkSize: vol.ChunkSize, PolicyVersion: vol.PolicyVersion,
		Inodes: stats.Inodes, Chunks: stats.Chunks, Tombstoned: stats.Tombstoned, LogicalBytes: stats.LogicalBytes, StoredBytes: stats.StoredBytes,
	}

	nodes := st.Nodes()
	probes := map[meta.NodeIdx]cluster.Probe{}
	if p != nil {
		probes = p.Snapshot()
	}
	// Chunk states come from the one derivation (meta.ChunkState, I-56) with the
	// repairer's fill_after, so status, check and the scanner agree.
	var lv meta.Liveness
	if p != nil {
		lv = p
	} else {
		lv = selfLiveness{self: n.ID, nodes: nodes}
	}
	fillAfter := cluster.FillAfter
	if rp != nil {
		fillAfter = rp.Config().FillAfter
	}
	health := ScanHealth(st, lv, fillAfter)
	rep.Volume.Degraded, rep.Volume.RepairPending, rep.Volume.Unreadable, rep.Volume.StalePolicy = health.Degraded, health.RepairPending, health.Unreadable, health.StalePolicy
	rep.Volume.DomainSkew, rep.Volume.DiskSkew = health.DomainSkew, health.DiskSkew
	rep.Volume.ToleratesDisks, rep.Volume.ToleratesHosts = meta.Tolerance(nodes, vol.K, vol.M)
	slots, missing := health.Slots, health.Missing
	live := map[meta.NodeIdx]bool{}
	for i := range nodes {
		live[nodes[i].Idx] = lv.IsLive(nodes[i].Idx)
	}
	_, leaderID := n.Raft.LeaderWithID()
	selfIdx, _ := n.SelfIdx()
	for i := range nodes {
		nd := nodes[i]
		self := nd.ID == n.ID || (nd.Kind == meta.KindDisk && nd.Host == selfIdx) // a disk of this host
		r := NodeReport{
			Idx: nd.Idx, ID: nd.ID, Name: nd.Name, Kind: nd.Kind.String(), Host: nd.Host, State: nd.State.String(), Addr: nd.Addr, RPCPort: nd.RPCPort,
			Voter: nd.Voter, Leader: nd.ID == string(leaderID), Self: self, Live: live[nd.Idx], Capacity: nd.Capacity, Version: nd.Version,
			Slots: slots[nd.Idx], Missing: missing[nd.Idx], TSStable: nd.TSStableID, JoinedAt: time.Unix(0, nd.JoinedAt),
		}
		if nd.Kind == meta.KindS3 {
			r.S3 = &S3Report{Endpoint: nd.S3Endpoint, Bucket: nd.S3Bucket, Prefix: nd.S3Prefix, Region: nd.S3Region, PathStyle: nd.S3PathStyle}
		}
		if p != nil {
			r.Suspect = p.IsSuspect(nd.Idx)
		}
		if pr, ok := probes[nd.Idx]; ok {
			r.fs = pr.FS
			r.Free, r.Applied, r.Dev = pr.Free, pr.Applied, pr.Dev
			if !pr.LastOK.IsZero() {
				r.RTT = pr.RTT.Round(time.Millisecond).String()
			}
			if !pr.DeadSince.IsZero() {
				r.DeadFor = now.Sub(pr.DeadSince).Round(time.Second).String()
				r.Lost = pr.Lost
			}
		}
		if nd.State == meta.Active && nd.Kind != meta.KindDevice { // a device stores nothing (docs/14 §9)
			rep.Volume.Capacity += nd.Capacity
		}
		rep.Nodes = append(rep.Nodes, r)
	}
	sort.Slice(rep.Nodes, func(i, j int) bool { return rep.Nodes[i].Idx < rep.Nodes[j].Idx })
	var used uint64
	rep.Volume.UsableCapacity, used, rep.Volume.Free = meta.Usage(vol, nodes, stats)
	if used > 0 {
		rep.Volume.DedupRatio = float64(stats.LogicalBytes) / float64(used)
	}
	if rp != nil && n.IsLeader() {
		st, at := rp.Last()
		rep.Repair = &RepairStatus{LastScan: at, Stats: st, Reported: rp.Reported()}
	}
	markDisks(rep.Nodes)
	rep.Warnings = warnings(rep)
	return rep
}

// markDisks flags each device with no disks as diskless (docs/14 §9), and lists the
// disks of one host on one filesystem with each other (docs/14 §8): they fail together.
func markDisks(nodes []NodeReport) {
	type key struct {
		host meta.NodeIdx
		fs   string
	}
	groups := map[key][]int{}
	hasDisk := map[meta.NodeIdx]bool{}
	for i := range nodes {
		n := &nodes[i]
		if n.State == meta.Removed.String() || n.Kind != meta.KindDisk.String() {
			continue
		}
		hasDisk[n.Host] = true
		if n.fs != "" {
			k := key{n.Host, n.fs}
			groups[k] = append(groups[k], i)
		}
	}
	for i := range nodes {
		if nodes[i].Kind == meta.KindDevice.String() && nodes[i].State != meta.Removed.String() && !hasDisk[nodes[i].Idx] {
			nodes[i].Diskless = true
		}
	}
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		for _, i := range members {
			for _, j := range members {
				if i != j {
					nodes[i].SharedWith = append(nodes[i].SharedWith, nodes[j].Name)
				}
			}
			sort.Strings(nodes[i].SharedWith)
		}
	}
}

// sharedDiskWarnings is one line per group of targets on one physical disk.
func sharedDiskWarnings(nodes []NodeReport) []string {
	seen := map[string]bool{}
	var out []string
	for i := range nodes {
		n := &nodes[i]
		if len(n.SharedWith) == 0 || seen[n.Name] {
			continue
		}
		names := append([]string{n.Name}, n.SharedWith...)
		sort.Strings(names)
		for _, name := range names {
			seen[name] = true
		}
		list := strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
		out = append(out, list+" are on one physical disk: they fail together, so data spread across them is not redundant")
	}
	return out
}

func warnings(r Report) []string {
	var w []string
	switch {
	case r.Voters == 1:
		w = append(w, "only one voter: metadata has no redundancy; promote an always-on device")
	case r.Voters%2 == 0:
		w = append(w, "even number of voters: a tie can lose quorum; use an odd count")
	}
	if r.Health != "healthy" {
		w = append(w, "metadata service is "+r.Health+": metadata writes are refused until a leader is reachable")
	}
	if r.Volume.Unreadable > 0 {
		w = append(w, "some chunks have fewer than k live shards and cannot be read right now")
	}
	for i := range r.Nodes {
		if r.Nodes[i].Dev && r.Nodes[i].Kind != "disk" { // a disk reports its host's flag
			w = append(w, "node "+r.Nodes[i].Name+" has the /dev/* endpoints enabled (dev.enabled): test rigs only")
		}
	}
	if r.Volume.Degraded > 0 {
		w = append(w, "some chunks are under-replicated; repair fills them once a live spare exists")
	}
	w = append(w, sharedDiskWarnings(r.Nodes)...)
	for i := range r.Nodes {
		n := &r.Nodes[i]
		if n.State == "removed" || n.Diskless {
			continue // a diskless host holds no slot: nothing to re-place, nothing to do (docs/14 §9)
		}
		switch {
		case n.Lost:
			w = append(w, fmt.Sprintf("node %s has been unreachable for %s, longer than evict_after: `tailraid node remove %d --force` drops its slots so repair can re-place them", n.Name, n.DeadFor, n.Idx))
		case !n.Live && n.DeadFor != "":
			w = append(w, "node "+n.Name+" has been unreachable for "+n.DeadFor)
		}
	}
	return w
}

// selfLiveness is the view of a node without a prober: only this node is live.
type selfLiveness struct {
	self  string
	nodes []meta.Node
}

func (s selfLiveness) IsLive(idx meta.NodeIdx) bool {
	for i := range s.nodes {
		if s.nodes[i].Idx == idx {
			return s.nodes[i].ID == s.self
		}
	}
	return false
}

func (selfLiveness) DeadFor(meta.NodeIdx) time.Duration { return 0 }
