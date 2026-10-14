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
	// HeaderBusy marks a 503 a shard request got because every slot stayed taken for
	// SlotWait (docs/05 §6, I-246).
	HeaderBusy = "X-Tailraid-Busy"
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
	// ErrBusy is a shard request a node refused because its slots stayed taken (I-246);
	// it comes wrapped in ErrUnavailable, so a reader or flusher moves to another slot,
	// and it never marks the node suspect.
	ErrBusy = errors.New("rpc: node busy")
	// ErrApplyFailed is a 500 from POST /v1/apply: the leader handed the command to Raft and
	// then failed (leadership lost, timeout); the outcome is unknown (docs/02 §forwarding).
	ErrApplyFailed = errors.New("rpc: apply failed on the leader after dispatch")
)

// Health is the unauthenticated GET /health body.
type Health struct {
	NodeID    string         `json:"node_id"`
	ClusterID string         `json:"cluster_id"`
	Version   string         `json:"version"`
	Applied   uint64         `json:"applied"`
	Codec     uint8          `json:"codec,omitempty"` // meta.CodecVersion of this binary (docs/12 §6)
	Ready     bool           `json:"ready"`           // a leader is known and the FSM has every committed entry (I-125)
	Leader    bool           `json:"leader"`
	Dev       bool           `json:"dev,omitempty"`     // /dev/* endpoints enabled on this node
	Web       uint16         `json:"web,omitempty"`     // the browser UI's port (docs/13 §1), 0 when off: for the dashboard's member links
	Disks     []DiskHealth   `json:"disks,omitempty"`   // this host's disks (docs/14 §4): the whole storage picture; a device stores nothing
	Mirrors   []MirrorHealth `json:"mirrors,omitempty"` // this host's mirrors (docs/16 §1): online on every node only while listed online here
	// SeenFrom is the UDP endpoint this node's device currently sees for the caller: the
	// caller's reflexive address, a candidate it publishes (docs/20 §5, I-322).
	SeenFrom string `json:"seen_from,omitempty"`
}

// MirrorHealth is one mirror of the host in /health (docs/16 §1): ID is the mirror's
// marker ID, the row's ID.
type MirrorHealth struct {
	ID      string      `json:"id"`
	Online  bool        `json:"online"`
	Reason  string      `json:"reason,omitempty"` // why it is offline
	Used    int64       `json:"used"`
	Free    uint64      `json:"free,omitempty"`
	Total   uint64      `json:"total,omitempty"`
	Syncing *SyncHealth `json:"syncing,omitempty"`
}

// SyncHealth is the snapshot a mirror's host is copying: the file bytes walked of the
// snapshot's total, and the bytes written to the mirror so far.
type SyncHealth struct {
	ID       uint32 `json:"id"`
	Snapshot string `json:"snapshot"`
	Read     uint64 `json:"read"`
	Total    uint64 `json:"total"`
	BytesNew uint64 `json:"bytes_new"`
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
	Token      string `json:"token,omitempty"` // under Tailscale; on the mesh the introduce consumed it (docs/20 §4)
	NodeID     string `json:"node_id"`
	Name       string `json:"name"`
	TSStableID string `json:"ts_stable_id,omitempty"` // under Tailscale
	Addr       string `json:"addr"`
	RaftPort   uint16 `json:"raft_port"`
	RPCPort    uint16 `json:"rpc_port"`
	Voter      bool   `json:"voter"`
	Version    string `json:"version"`
	// The mesh (docs/20 §4): the joiner's key (base64), its UDP port and the endpoint
	// the member saw its introduce from, its first candidate.
	Key      string `json:"key,omitempty"`
	MeshPort uint16 `json:"mesh_port,omitempty"`
	SeenFrom string `json:"seen_from,omitempty"`
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
	Op        string         `json:"op"`                // remove | update_addr | update_endpoints | set_version | set_voter | set_capacity | set_state | add_disk | add_s3
	NodeIdx   meta.NodeIdx   `json:"node_idx"`          // add_disk: the host
	DiskID    string         `json:"disk_id,omitempty"` // add_disk: the marker's disk ID (docs/14 §2)
	Name      string         `json:"name,omitempty"`    // add_disk: "<host>/<n>"; add_s3: the S3 node's name
	Addr      string         `json:"addr,omitempty"`
	Version   string         `json:"version,omitempty"` // set_version: what the node's serve runs (I-133)
	Voter     bool           `json:"voter,omitempty"`
	Capacity  uint64         `json:"capacity,omitempty"`
	State     meta.NodeState `json:"state,omitempty"`
	Force     bool           `json:"force,omitempty"`
	S3        *S3Spec        `json:"s3,omitempty"`        // add_s3
	Endpoints []string       `json:"endpoints,omitempty"` // update_endpoints: the caller's own candidates (docs/20 §5, I-322)
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
	Op     string `json:"op"`               // repair_now | gc_now | set_policy | check | fsck | chunk | rotate_token | open_disk | export (was snapshot) | trash_ls | trash_restore | trash_purge | cache_pin | cache_unpin | cache_ls | stats_seal | rebalance | snapshot_ls | snapshot_create | snapshot_restore | versions | versions_revert | versions_copy (docs/16: Name, Mirror, Path, To, As; no op removes a snapshot, I-183)
	Mirror string `json:"mirror,omitempty"` // snapshot_create, snapshot_ls: one mirror by name ("" every one)
	Name   string `json:"name,omitempty"`   // snapshot_create: the snapshot's name ("" the UTC minute)
	K      uint8  `json:"k,omitempty"`
	M      uint8  `json:"m,omitempty"`
	Auto   bool   `json:"auto,omitempty"`  // set_policy: hand the policy back to the leader (docs/03)
	Scrub  bool   `json:"scrub,omitempty"` // check: force a full scrub pass
	Files  bool   `json:"files,omitempty"` // check: list affected files
	Chunk  string `json:"chunk,omitempty"` // chunk: hex hash (prefix ≥ 8) or a /path
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
	// trash_ls a page at a time (I-234): at most PageBytes of entries after Cursor ("" the
	// first page); the reply's "next" is the following page's cursor. PageBytes 0 lists
	// every entry in one reply, as a CLI before paging asks.
	Cursor    string `json:"cursor,omitempty"`
	PageBytes int    `json:"page_bytes,omitempty"`
	All       bool   `json:"all,omitempty"`
	Rollup    bool   `json:"rollup,omitempty"`  // stats_seal: also write today's rollups so far (docs/17 §7)
	DryRun    bool   `json:"dry_run,omitempty"` // rebalance: list the home moves, open nothing (docs/14 §7)
	// search (docs/18 §6, this node's index): the words, the folder to search under (Path)
	// and the most hits to return; index_now runs the leader's pass (a pass op, I-189); scrape_plan (docs/18 §11)
	// lists the items under Path missing a sidecar, at most Limit after Cursor
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
	// secret_set, secret_get, secret_ls, secret_rm (docs/19 §5): the secret's name (Name)
	// and, for secret_set, its value, which no node logs (I-306); All with secret_rm
	// removes every secret and the key ID
	Value string `json:"value,omitempty"`
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
