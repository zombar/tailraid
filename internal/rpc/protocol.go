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
	// HeaderPriority marks a shard request as background work (the repairer's), which
	// holds at most half of a node's PUT and GET slots (docs/06 §4, I-175).
	HeaderPriority     = "X-Tailraid-Priority"
	PriorityBackground = "background"
)

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
	Ready     bool         `json:"ready"`           // a leader is known and the FSM has every committed entry (I-125)
	Leader    bool         `json:"leader"`
	Dev       bool         `json:"dev,omitempty"`   // /dev/* endpoints enabled on this node
	Web       uint16       `json:"web,omitempty"`   // the browser UI's port (docs/13 §1), 0 when off: for the dashboard's member links
	Disks     []DiskHealth `json:"disks,omitempty"` // this host's disks (docs/14 §4): the whole storage picture; a device stores nothing
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
	Version    string `json:"version"`
}

// JoinResponse is the join reply.
type JoinResponse struct {
	ClusterID string       `json:"cluster_id"`
	NodeIdx   meta.NodeIdx `json:"node_idx"`
	Volume    meta.Volume  `json:"volume"`
	Members   []meta.Node  `json:"members"`
	Disks     []meta.Node  `json:"disks,omitempty"` // set by the joiner: the disks it registered from its config (docs/14 §2)
}

// MemberOp is POST /v1/members.
type MemberOp struct {
	Op       string         `json:"op"`                // remove | update_addr | set_version | set_voter | set_capacity | set_state | add_disk | add_s3
	NodeIdx  meta.NodeIdx   `json:"node_idx"`          // add_disk: the host
	DiskID   string         `json:"disk_id,omitempty"` // add_disk: the marker's disk ID (docs/14 §2)
	Name     string         `json:"name,omitempty"`    // add_disk: "<host>/<n>"; add_s3: the S3 node's name
	Addr     string         `json:"addr,omitempty"`
	Version  string         `json:"version,omitempty"` // set_version: what the node's serve runs (I-133)
	Voter    bool           `json:"voter,omitempty"`
	Capacity uint64         `json:"capacity,omitempty"`
	State    meta.NodeState `json:"state,omitempty"`
	Force    bool           `json:"force,omitempty"`
	S3       *S3Spec        `json:"s3,omitempty"` // add_s3
}

// S3Spec is the bucket of an add_s3 (docs/10 §4.1). Credentials are never part of it (I-139).
type S3Spec struct {
	ID        string `json:"id"` // drawn by the CLI, so a retry updates the row it created
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	Region    string `json:"region,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
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
	Op    string `json:"op"` // repair_now | gc_now | set_policy | check | fsck | chunk | rotate_token | open_disk | snapshot | trash_ls | trash_restore | trash_purge | cache_pin | cache_unpin | cache_ls | stats_seal
	K     uint8  `json:"k,omitempty"`
	M     uint8  `json:"m,omitempty"`
	Auto  bool   `json:"auto,omitempty"`  // set_policy: hand the policy back to the leader (docs/03)
	Scrub bool   `json:"scrub,omitempty"` // check: force a full scrub pass
	Files bool   `json:"files,omitempty"` // check: list affected files
	Chunk string `json:"chunk,omitempty"` // chunk: hex hash (prefix ≥ 8) or a /path
	// open_disk (this host only): start serving a disk `node add-disk` has just added;
	// trash_ls: the folder to list (empty: every root); cache_pin and cache_unpin (this
	// host only): the folder to keep on this device, or release (docs/12 §10)
	Path     string `json:"path,omitempty"`
	Capacity string `json:"capacity,omitempty"`
	// the trash (docs/12 §10): trash_restore takes the root's inode and an optional
	// destination folder and name; trash_purge takes one inode, or everything deleted
	// longer than OlderThan ago ("" with All: everything)
	Ino       uint64 `json:"ino,omitempty"`
	To        string `json:"to,omitempty"`
	As        string `json:"as,omitempty"`
	OlderThan string `json:"older_than,omitempty"`
	All       bool   `json:"all,omitempty"`
	Rollup    bool   `json:"rollup,omitempty"` // stats_seal: also write today's rollups so far (docs/17 §7)
	// search (docs/18 §6, this node's index): the words, the folder to search under (Path)
	// and the most hits to return; index_now runs the leader's pass (a pass op, I-189)
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// IsPass reports an op the leader answers only once a pass over the volume is done
// (docs/08, I-189): the client gives it no deadline of its own.
func (op AdminOp) IsPass() bool {
	switch op.Op {
	case "repair_now", "gc_now", "index_now":
		return true
	case "check":
		return op.Scrub
	case "trash_purge": // the whole trash: one proposal after another until it is empty
		return op.All
	}
	return false
}
