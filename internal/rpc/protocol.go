// Package rpc is the node-to-node HTTP transport over the tailnet (docs/02, docs/06,
// docs/08): shard bodies are raw bytes, control messages are small JSON documents.
package rpc

import (
	"errors"

	"github.com/zombar/tailraid/internal/meta"
)

// Header names.
const (
	HeaderCluster  = "X-Tailraid-Cluster"   // hex cluster ID; mismatch → 409
	HeaderLogIndex = "X-Tailraid-Log-Index" // DELETE fence: GCDelete/SetPlacement index
	HeaderShardSum = "X-Shard-SHA256"       // optional hex sha256 of a PUT body
	HeaderIndex    = "X-Tailraid-Index"     // log index of an applied command
	HeaderLeader   = "X-Tailraid-Leader"    // leader RPC address on 421
	HeaderNodeID   = "X-Tailraid-Node"      // responding node's raft ID
)

// Ports.
const ()

// Errors the client returns.
var (
	ErrForbidden    = errors.New("rpc: forbidden")
	ErrRateLimited  = errors.New("rpc: too many join attempts from this source")
	ErrWrongCluster = errors.New("rpc: cluster id mismatch")
	ErrNotLeader    = errors.New("rpc: not the leader")
	ErrNoLeader     = errors.New("rpc: no leader")
	ErrFenced       = errors.New("rpc: delete refused by fence")
	ErrTooLarge     = errors.New("rpc: body too large")
	ErrBadRequest   = errors.New("rpc: bad request")
	ErrUnavailable  = errors.New("rpc: node unavailable")
	// ErrApplyFailed is a 500 from POST /v1/apply: the leader handed the command to Raft and
	// then failed (leadership lost, timeout); the outcome is unknown (docs/02 §forwarding).
	ErrApplyFailed = errors.New("rpc: apply failed on the leader after dispatch")
)

// Health is the unauthenticated GET /health body.
type Health struct {
	NodeID    string       `json:"node_id"`
	ClusterID string       `json:"cluster_id"`
	Version   string       `json:"version"`
	Applied   uint64       `json:"applied"`
	Codec     uint8        `json:"codec,omitempty"` // meta.CodecVersion of this binary (docs/12 §6)
	Leader    bool         `json:"leader"`
	Used      int64        `json:"used"`
	Capacity  int64        `json:"capacity"`
	Free      int64        `json:"free"`
	Dev       bool         `json:"dev,omitempty"`   // /dev/* endpoints enabled on this node
	FS        string       `json:"fs,omitempty"`    // filesystem holding disk 0, to spot targets on one physical disk (docs/14 §8)
	Disks     []DiskHealth `json:"disks,omitempty"` // added disks (docs/14 §4); the fields above are disk 0
}

// DiskHealth is one added disk in /health: OK is false while the disk is closed (its
// marker is missing or foreign) or failing, which makes it dead to every node (I-90).
type DiskHealth struct {
	ID       string `json:"id"`
	Used     int64  `json:"used"`
	Capacity int64  `json:"capacity"`
	Free     int64  `json:"free"`
	OK       bool   `json:"ok"`
	Reason   string `json:"reason,omitempty"`
	FS       string `json:"fs,omitempty"` // filesystem holding this disk
}

// JoinRequest is POST /v1/join.
type JoinRequest struct {
	Token      string `json:"token"`
	NodeID     string `json:"node_id"`
	Name       string `json:"name"`
	TSStableID string `json:"ts_stable_id"`
	Addr       string `json:"addr"`
	RaftPort   uint16 `json:"raft_port"`
	RPCPort    uint16 `json:"rpc_port"`
	Voter      bool   `json:"voter"`
	Capacity   uint64 `json:"capacity"`
	Version    string `json:"version"`
}

// JoinResponse is the join reply.
type JoinResponse struct {
	ClusterID string       `json:"cluster_id"`
	NodeIdx   meta.NodeIdx `json:"node_idx"`
	Volume    meta.Volume  `json:"volume"`
	Members   []meta.Node  `json:"members"`
}

// MemberOp is POST /v1/members.
type MemberOp struct {
	Op       string         `json:"op"`                // remove | update_addr | set_voter | set_capacity | set_state | add_disk
	NodeIdx  meta.NodeIdx   `json:"node_idx"`          // add_disk: the host
	DiskID   string         `json:"disk_id,omitempty"` // add_disk: the marker's disk ID (docs/14 §2)
	Name     string         `json:"name,omitempty"`    // add_disk: "<host>/<n>"
	Addr     string         `json:"addr,omitempty"`
	Voter    bool           `json:"voter,omitempty"`
	Capacity uint64         `json:"capacity,omitempty"`
	State    meta.NodeState `json:"state,omitempty"`
	Force    bool           `json:"force,omitempty"`
}

// BadSlot is POST /v1/bad-slot.
type BadSlot struct {
	Key  string       `json:"key"`
	Node meta.NodeIdx `json:"node"`
}

// ErrorBody is the JSON body of error responses.
type ErrorBody struct {
	Error string `json:"error"`
}

// AdminOp is POST /v1/admin: leader-side maintenance actions.
type AdminOp struct {
	Op    string `json:"op"` // repair_now | gc_now | set_policy | check | fsck | chunk | rotate_token | open_disk
	K     uint8  `json:"k,omitempty"`
	M     uint8  `json:"m,omitempty"`
	Auto  bool   `json:"auto,omitempty"`  // set_policy: hand the policy back to the leader (docs/03)
	Scrub bool   `json:"scrub,omitempty"` // check: force a full scrub pass
	Files bool   `json:"files,omitempty"` // check: list affected files
	Chunk string `json:"chunk,omitempty"` // chunk: hex hash (prefix ≥ 8) or a /path
	// open_disk (this host only): start serving a disk `node add-disk` has just added
	Path     string `json:"path,omitempty"`
	Capacity string `json:"capacity,omitempty"`
}
