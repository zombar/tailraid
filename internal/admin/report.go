// Package admin builds the reports behind `status`, `node`, `volume` and `/v1/status`.
package admin

import (
	"sort"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
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
	RTT      string         `json:"rtt,omitempty"`
	Free     int64          `json:"free"`
	Capacity uint64         `json:"capacity"`
	Version  string         `json:"version"`
	Applied  uint64         `json:"applied"`
	Slots    int            `json:"slots"`
	Missing  int            `json:"missing_slots"` // slots this node should hold but is dead for
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
	Unreadable     int     `json:"unreadable_chunks"` // chunks with < k live slots
	StalePolicy    int     `json:"stale_policy_chunks"`
}

// Report is the whole `status` document.
type Report struct {
	Node        string       `json:"node"`
	ClusterID   string       `json:"cluster_id"`
	Health      string       `json:"health"`
	Leader      string       `json:"leader"`
	LastContact string       `json:"last_contact,omitempty"`
	Voters      int          `json:"voters"`
	LiveVoters  int          `json:"live_voters"`
	Warnings    []string     `json:"warnings,omitempty"`
	Volume      VolumeReport `json:"volume"`
	Nodes       []NodeReport `json:"nodes"`
	JoinToken   string       `json:"join_token,omitempty"`
	GeneratedAt time.Time    `json:"generated_at"`
}

// Build assembles the report from the local FSM and prober view.
func Build(n *cluster.Node, p *cluster.Prober, clusterID [16]byte, clk clock.Clock) Report {
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
	live := map[meta.NodeIdx]bool{}
	slots := map[meta.NodeIdx]int{}
	missing := map[meta.NodeIdx]int{}
	for i := range nodes {
		if p != nil {
			live[nodes[i].Idx] = p.IsLive(nodes[i].Idx)
		} else {
			live[nodes[i].Idx] = nodes[i].ID == n.ID
		}
	}
	st.ForEachChunk(func(c meta.Chunk) bool {
		if c.Refs == 0 {
			return true
		}
		good, liveSlots := 0, 0
		for _, s := range c.Placement {
			if s == 0 {
				continue
			}
			slots[s]++
			good++
			if live[s] {
				liveSlots++
			} else {
				missing[s]++
			}
		}
		if good < int(c.K)+int(c.M) || liveSlots < int(c.K)+int(c.M) {
			rep.Volume.Degraded++
		}
		if liveSlots < int(c.K) {
			rep.Volume.Unreadable++
		}
		// small chunks use k=1 by design; anything else on a different (k,m) awaits migration
		if (c.K != vol.K || c.M != vol.M) && (c.K != 1 || c.Size > 64<<10) {
			rep.Volume.StalePolicy++
		}
		return true
	})
	_, leaderID := n.Raft.LeaderWithID()
	for i := range nodes {
		nd := nodes[i]
		r := NodeReport{Idx: nd.Idx, ID: nd.ID, Name: nd.Name, Kind: kindName(nd.Kind), State: stateName(nd.State), Addr: nd.Addr, RPCPort: nd.RPCPort,
			Voter: nd.Voter, Leader: nd.ID == string(leaderID), Self: nd.ID == n.ID, Live: live[nd.Idx], Capacity: nd.Capacity, Version: nd.Version,
			Slots: slots[nd.Idx], Missing: missing[nd.Idx], TSStable: nd.TSStableID, JoinedAt: time.Unix(0, nd.JoinedAt), rawState: nd.State}
		if pr, ok := probes[nd.Idx]; ok {
			r.Free, r.Applied = pr.Free, pr.Applied
			if !pr.LastOK.IsZero() {
				r.RTT = pr.RTT.Round(time.Millisecond).String()
			}
			if !pr.DeadSince.IsZero() {
				r.DeadFor = now.Sub(pr.DeadSince).Round(time.Second).String()
			}
		}
		if nd.State == meta.Active {
			rep.Volume.Capacity += nd.Capacity
		}
		rep.Nodes = append(rep.Nodes, r)
	}
	sort.Slice(rep.Nodes, func(i, j int) bool { return rep.Nodes[i].Idx < rep.Nodes[j].Idx })
	k, km := uint64(vol.K), uint64(vol.K)+uint64(vol.M)
	if km > 0 {
		rep.Volume.UsableCapacity = rep.Volume.Capacity * k / km
		used := stats.StoredBytes * k / km
		if rep.Volume.UsableCapacity > used {
			rep.Volume.Free = rep.Volume.UsableCapacity - used
		}
		if used > 0 {
			rep.Volume.DedupRatio = float64(stats.LogicalBytes) / float64(used)
		}
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
	if r.Volume.Degraded > 0 {
		w = append(w, "some chunks are under-replicated; repair fills them once a live spare exists")
	}
	for i := range r.Nodes {
		n := &r.Nodes[i]
		if n.State == "removed" {
			continue
		}
		if !n.Live && n.DeadFor != "" {
			w = append(w, "node "+n.Name+" has been unreachable for "+n.DeadFor)
		}
	}
	return w
}

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
