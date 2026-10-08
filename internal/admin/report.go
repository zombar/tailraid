// Package admin builds the reports behind `status`, `node`, `volume` and `/v1/status`.
package admin

import (
	"fmt"
	"sort"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/vfs"
)

// NodeReport is one row of `node ls`.
type NodeReport struct {
	Idx      meta.NodeIdx   `json:"idx"`
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	State    string         `json:"state"`
	Addr     string         `json:"addr"`
	RPCPort  uint16         `json:"rpc_port"`
	Voter    bool           `json:"voter"`
	Leader   bool           `json:"leader"`
	Self     bool           `json:"self"`
	Live     bool           `json:"live"`
	DeadFor  string         `json:"dead_for,omitempty"`
	Lost     bool           `json:"lost,omitempty"` // dead longer than evict_after (MemberLost): remove it
	RTT      string         `json:"rtt,omitempty"`
	Free     int64          `json:"free"`
	Capacity uint64         `json:"capacity"`
	Version  string         `json:"version"`
	Applied  uint64         `json:"applied"`
	Slots    int            `json:"slots"`
	Missing  int            `json:"missing_slots"` // slots this node should hold but is dead for
	Dev      bool           `json:"dev,omitempty"` // /dev/* endpoints enabled
	TSStable string         `json:"ts_stable_id,omitempty"`
	JoinedAt time.Time      `json:"joined_at"`
	rawState meta.NodeState // for sorting
}

// VolumeReport is `volume describe`.
type VolumeReport struct {
	K              uint8   `json:"k"`
	M              uint8   `json:"m"`
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
}

// GatewayReport is this node's overlay: what is acknowledged but not yet committed.
type GatewayReport struct {
	DirtyInodes int    `json:"dirty_inodes"`
	DirtyBytes  uint64 `json:"dirty_bytes"` // bytes in extents not yet captured by a flush plan
	WALBytes    int64  `json:"wal_bytes"`   // Σ per-inode WAL file sizes
	Flushing    int    `json:"flushing"`    // inodes with a flush plan outstanding
}

// GatewayFromDirty folds the overlay's dirty list into the status view.
func GatewayFromDirty(infos []vfs.DirtyInfo) *GatewayReport {
	g := &GatewayReport{DirtyInodes: len(infos)}
	for i := range infos {
		g.DirtyBytes += infos[i].DirtyBytes
		g.WALBytes += infos[i].WALBytes
		if infos[i].Flushing {
			g.Flushing++
		}
	}
	return g
}

// Report is the whole `status` document.
type Report struct {
	Node        string         `json:"node"`
	ClusterID   string         `json:"cluster_id"`
	Health      string         `json:"health"`
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
	GeneratedAt time.Time      `json:"generated_at"`
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
	rep := Report{Node: n.ID, ClusterID: hexID(clusterID), GeneratedAt: now}
	h := n.Health(clk, p)
	rep.Health, rep.Leader, rep.Voters, rep.LiveVoters = h.State.String(), h.Leader, h.Voters, h.LiveVoters
	if h.LastContact > 0 {
		rep.LastContact = h.LastContact.Round(time.Millisecond).String()
	}
	rep.JoinToken = st.Meta().JoinToken

	vol := st.Volume()
	stats := st.Stats()
	rep.Volume = VolumeReport{K: vol.K, M: vol.M, ChunkSize: vol.ChunkSize, PolicyVersion: vol.PolicyVersion,
		Inodes: stats.Inodes, Chunks: stats.Chunks, Tombstoned: stats.Tombstoned, LogicalBytes: stats.LogicalBytes, StoredBytes: stats.StoredBytes}

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
	slots, missing := health.Slots, health.Missing
	live := map[meta.NodeIdx]bool{}
	for i := range nodes {
		live[nodes[i].Idx] = lv.IsLive(nodes[i].Idx)
	}
	_, leaderID := n.Raft.LeaderWithID()
	for i := range nodes {
		nd := nodes[i]
		r := NodeReport{Idx: nd.Idx, ID: nd.ID, Name: nd.Name, Kind: kindName(nd.Kind), State: stateName(nd.State), Addr: nd.Addr, RPCPort: nd.RPCPort,
			Voter: nd.Voter, Leader: nd.ID == string(leaderID), Self: nd.ID == n.ID, Live: live[nd.Idx], Capacity: nd.Capacity, Version: nd.Version,
			Slots: slots[nd.Idx], Missing: missing[nd.Idx], TSStable: nd.TSStableID, JoinedAt: time.Unix(0, nd.JoinedAt), rawState: nd.State}
		if pr, ok := probes[nd.Idx]; ok {
			r.Free, r.Applied, r.Dev = pr.Free, pr.Applied, pr.Dev
			if !pr.LastOK.IsZero() {
				r.RTT = pr.RTT.Round(time.Millisecond).String()
			}
			if !pr.DeadSince.IsZero() {
				r.DeadFor = now.Sub(pr.DeadSince).Round(time.Second).String()
				r.Lost = pr.Lost
			}
		}
		if nd.State == meta.Active {
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
	rep.Warnings = warnings(rep)
	return rep
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
		if r.Nodes[i].Dev {
			w = append(w, "node "+r.Nodes[i].Name+" has the /dev/* endpoints enabled (dev.enabled): test rigs only")
		}
	}
	if r.Volume.Degraded > 0 {
		w = append(w, "some chunks are under-replicated; repair fills them once a live spare exists")
	}
	for i := range r.Nodes {
		n := &r.Nodes[i]
		if n.State == "removed" {
			continue
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

func kindName(k meta.NodeKind) string {
	if k == meta.KindS3 {
		return "s3"
	}
	return "device"
}

func stateName(s meta.NodeState) string {
	switch s {
	case meta.Draining:
		return "draining"
	case meta.Removed:
		return "removed"
	}
	return "active"
}

func hexID(id [16]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 32)
	for i, b := range id {
		out[i*2], out[i*2+1] = digits[b>>4], digits[b&0xf]
	}
	return string(out)
}
