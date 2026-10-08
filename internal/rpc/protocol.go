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
const (
	DefaultRaftPort = 20480
	DefaultRPCPort  = 20481
)

// Errors the client returns.
var (
	ErrForbidden    = errors.New("rpc: forbidden")
	ErrWrongCluster = errors.New("rpc: cluster id mismatch")
	ErrNotLeader    = errors.New("rpc: not the leader")
	ErrNoLeader     = errors.New("rpc: no leader")
	ErrFenced       = errors.New("rpc: delete refused by fence")
	ErrTooLarge     = errors.New("rpc: body too large")
	ErrBadRequest   = errors.New("rpc: bad request")
	ErrUnavailable  = errors.New("rpc: node unavailable")
)

// Health is the unauthenticated GET /health body.
type Health struct {
	NodeID    string `json:"node_id"`
	ClusterID string `json:"cluster_id"`
	Version   string `json:"version"`
	Applied   uint64 `json:"applied"`
	Leader    bool   `json:"leader"`
	Used      int64  `json:"used"`
	Capacity  int64  `json:"capacity"`
	Free      int64  `json:"free"`
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
	Op       string         `json:"op"` // remove | update_addr | set_voter | set_capacity | set_state
	NodeIdx  meta.NodeIdx   `json:"node_idx"`
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
	Op    string `json:"op"` // repair_now | gc_now | set_policy | check | fsck
	K     uint8  `json:"k,omitempty"`
	M     uint8  `json:"m,omitempty"`
	Scrub bool   `json:"scrub,omitempty"` // check: force a full scrub pass
	Files bool   `json:"files,omitempty"` // check: list affected files
}
