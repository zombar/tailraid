// Package rpc is the node-to-node HTTP transport over the tailnet (docs/02, docs/06,
// docs/08): shard bodies are raw bytes, control messages are small JSON documents.
package rpc

import (
	"errors"
	"net/netip"
	"sort"

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
	// HeaderBlockTable is the length in bytes of the block table that opens a 206 reply
	// to a shard GET with a Range (docs/04 §5, I-423).
	HeaderBlockTable = "X-Tailraid-Block-Table"
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
	// ErrUnauthorized is a 401: the request needed a user's signature and carried none the
	// node accepts (docs/21 §2, I-344).
	ErrUnauthorized = errors.New("rpc: unauthorized: this needs a signed request from a user of the volume (docs/21)")
	// ErrBusy is a shard request a node refused because its slots stayed taken (I-246);
	// it comes wrapped in ErrUnavailable, so a reader or flusher moves to another slot,
	// and it never marks the node suspect.
	ErrBusy = errors.New("rpc: node busy")
	// ErrApplyFailed is a 500 from POST /v1/apply: the leader handed the command to Raft and
	// then failed (leadership lost, timeout); the outcome is unknown (docs/02 §forwarding).
	ErrApplyFailed = errors.New("rpc: apply failed on the leader after dispatch")
	// ErrNoRestore is a restore job the host does not list, or one that ended.
	ErrNoRestore = errors.New("rpc: no such restore job")
	// ErrNotHost is a restore started on a node that does not host the mirror.
	ErrNotHost = errors.New("rpc: this node does not host the mirror")
	// ErrNoRestoreJobs is a host whose version has no restore jobs (it answers their
	// routes with a bare 404, docs/16 §6).
	ErrNoRestoreJobs = errors.New("rpc: the mirror's host runs a version without restore jobs; update it")
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
	Dev       bool           `json:"dev,omitempty"`      // /dev/* endpoints enabled on this node
	Web       uint16         `json:"web,omitempty"`      // the browser UI's port (docs/13 §1), 0 when off: for the dashboard's member links
	Disks     []DiskHealth   `json:"disks,omitempty"`    // this host's disks (docs/14 §4): the whole storage picture; a device stores nothing
	Mirrors   []MirrorHealth `json:"mirrors,omitempty"`  // this host's mirrors (docs/16 §1): online on every node only while listed online here
	Restores  int            `json:"restores,omitempty"` // how many restore jobs this host lists (docs/16 §6): a count, since /health is open to the tailnet
}

// RestoreJob is one restore job of a mirror's host (docs/16 §6), as GET /v1/restores and
// status.restores list it. Times are Unix nanoseconds; Rate is bytes a second over the
// last minute.
type RestoreJob struct {
	ID         string       `json:"id"` // "<host idx>-<n>", unique in the cluster
	Host       string       `json:"host"`
	Mirror     string       `json:"mirror"` // the mirror row's name (pike/m1)
	MirrorIdx  meta.NodeIdx `json:"mirror_idx"`
	Snapshot   string       `json:"snapshot"`
	SnapID     meta.SnapID  `json:"snap_id"`
	Src        string       `json:"src"` // the path inside the snapshot
	Dst        string       `json:"dst"`
	Actor      meta.UserIdx `json:"actor"`
	User       string       `json:"user,omitempty"`
	State      string       `json:"state"`
	Error      string       `json:"error,omitempty"`
	Counted    bool         `json:"counted,omitempty"` // the totals are known
	FilesTotal int64        `json:"files_total"`
	BytesTotal int64        `json:"bytes_total"`
	FilesDone  int64        `json:"files_done"`
	BytesDone  int64        `json:"bytes_done"`
	Existing   int64        `json:"existing"`
	Rate       int64        `json:"rate"`
	Tries      int          `json:"tries,omitempty"`   // failed attempts since the last progress
	Resumed    int          `json:"resumed,omitempty"` // starts after a restart of the host
	Started    int64        `json:"started"`
	Updated    int64        `json:"updated"`
	Ended      int64        `json:"ended,omitempty"`
	NextTry    int64        `json:"next_try,omitempty"`
	Yielding   bool         `json:"yielding,omitempty"` // narrowed while clients are busy (I-372)
}

// The states of a restore job (docs/16 §6).
const (
	RestoreCounting  = "counting"
	RestoreQueued    = "queued"
	RestoreRunning   = "running"
	RestoreWaiting   = "waiting"
	RestoreRetrying  = "retrying"
	RestoreDone      = "done"
	RestoreFailed    = "failed"
	RestoreCancelled = "cancelled"
)

// Finished reports a job that runs no more.
func (j *RestoreJob) Finished() bool {
	return j.State == RestoreDone || j.State == RestoreFailed || j.State == RestoreCancelled
}

// RestoreStart is POST /v1/restores: a tree restore of path in snapshot of mirror into
// dst, for actor, sent to the mirror's host by the node a user asked (docs/16 §6).
type RestoreStart struct {
	Mirror   meta.NodeIdx `json:"mirror"`
	Snapshot meta.SnapID  `json:"snapshot"`
	Path     string       `json:"path"`
	Dst      string       `json:"dst"`
	Actor    meta.UserIdx `json:"actor"`
}

// RestoreStarted is the host's answer once the job is recorded.
type RestoreStarted struct {
	Job  string `json:"job"`
	Host string `json:"host"`
	Path string `json:"path"`
}

// RestoreCancel is POST /v1/restores/{id}/cancel: the user who asks.
type RestoreCancel struct {
	Actor meta.UserIdx `json:"actor"`
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
	// Actor is set by the receiving node from the request's signature (docs/21 §2).
	Actor meta.UserIdx `json:"-"`
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
	Op     string `json:"op"`               // repair_now | gc_now | set_policy | check | fsck | chunk | rotate_token | open_disk | export (was snapshot) | trash_ls | trash_restore | trash_purge | cache_pin | cache_unpin | cache_ls | stats_seal | rebalance | snapshot_ls | snapshot_create | snapshot_restore | restore_cancel | versions | versions_revert | versions_copy (docs/16: Name, Mirror, Path, To, As; no op removes a snapshot, I-183)
	Mirror string `json:"mirror,omitempty"` // snapshot_create, snapshot_ls: one mirror by name ("" every one)
	Job    string `json:"job,omitempty"`    // restore_cancel: the restore job (docs/16 §6)
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
	// lists the items under Path missing a sidecar, at most Limit after Cursor (All: also
	// those answered "no match"); scrape_skip remembers the item Ino of Kind at Path as
	// "no match" (I-394)
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
	// secret_set, secret_get, secret_ls, secret_rm (docs/19 §5): the secret's name (Name)
	// and, for secret_set, its value, which no node logs (I-306); All with secret_rm
	// removes every secret and the key ID
	Value string `json:"value,omitempty"`
	// Users, groups and permissions (docs/21 §9): the user or group an op names, a role,
	// an invite code, a public key (hex) with its label, a credential to bind, and the
	// admin flag of an invite. Code and Key are never logged.
	User       string `json:"user,omitempty"`
	Group      string `json:"group,omitempty"`
	Role       string `json:"role,omitempty"`
	Code       string `json:"code,omitempty"`
	Key        string `json:"key,omitempty"`
	Label      string `json:"label,omitempty"`
	Credential string `json:"credential,omitempty"`
	Admin      bool   `json:"admin,omitempty"`
	// audit_ls (docs/21 §8): records since a time (RFC 3339) of a kind, at most Limit
	Since string `json:"since,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// Actor and Source are set by the receiving node, from the request's signature and
	// its connection, never from the body (docs/21 §2, I-344).
	Actor   meta.UserIdx   `json:"-"`
	SignKey meta.UserKeyID `json:"-"` // the key that signed the request: a session it mints is its own (docs/21 §6)
	Source  netip.Addr     `json:"-"`
}

// Need is what an admin op asks of its caller (docs/21 §2, I-345): nothing beyond
// membership, a user, Write or Admin on the inode the op's path names, or admins.
type Need uint8

// Needs.
const (
	NeedNone Need = iota
	NeedUser
	NeedWriteOn
	NeedAdminOn
	NeedAdmins
)

// adminNeeds is the one table of every admin op (I-345); an op absent from it is
// refused before anything runs. The node refines a few: `check` with a scrub,
// `trash_purge` of everything, `secret_ls` of the system names, `secret_get` of a system
// name and a key invite for another user all need admins.
var adminNeeds = map[string]Need{
	"chunk": NeedNone, "trash_ls": NeedNone, "cache_ls": NeedNone, "search": NeedNone, "snapshot_ls": NeedNone,
	"versions": NeedNone, "scrape_plan": NeedNone, "fsck": NeedNone, "check": NeedNone,
	"access_ls": NeedNone, "user_ls": NeedNone, "group_ls": NeedNone, "user_create": NeedNone, "user_key_add": NeedNone, "user_bootstrap": NeedNone,
	"secret_set": NeedUser, "secret_get": NeedUser, "secret_ls": NeedUser, "secret_rm": NeedUser,
	"user_whoami": NeedUser, "user_key_invite": NeedUser, "user_key_rm": NeedUser, "user_bind": NeedUser, "user_unbind": NeedUser,
	"web_login": NeedUser, "web_logout": NeedUser, "audit_ls": NeedUser,
	"trash_restore": NeedWriteOn, "scrape_skip": NeedWriteOn,
	"trash_purge": NeedAdminOn, "cache_pin": NeedAdminOn, "cache_unpin": NeedAdminOn, "snapshot_restore": NeedAdminOn, "restore_cancel": NeedUser,
	"versions_revert": NeedAdminOn, "versions_copy": NeedAdminOn, "grant": NeedAdminOn, "revoke": NeedAdminOn,
	"repair_now": NeedAdmins, "gc_now": NeedAdmins, "index_now": NeedAdmins, "rebalance": NeedAdmins, "set_policy": NeedAdmins,
	"rotate_token": NeedAdmins, "open_disk": NeedAdmins, "open_mirror": NeedAdmins, "snapshot_create": NeedAdmins,
	"export": NeedAdmins, "snapshot": NeedAdmins, "stats_seal": NeedAdmins,
	"user_invite": NeedAdmins, "user_disable": NeedAdmins, "user_enable": NeedAdmins, "user_rm": NeedAdmins,
	"group_create": NeedAdmins, "group_rm": NeedAdmins, "group_add": NeedAdmins, "group_remove": NeedAdmins,
}

// Needs is what op asks of its caller; known is false for an op the table lacks.
func (op AdminOp) Needs() (need Need, known bool) {
	need, known = adminNeeds[op.Op]
	switch {
	case op.Op == "check" && op.Scrub, op.Op == "trash_purge" && op.All, op.Op == "secret_ls" && op.All:
		need = NeedAdmins
	}
	return need, known
}

// AdminOpNames lists every admin op the table knows, sorted.
func AdminOpNames() []string {
	out := make([]string, 0, len(adminNeeds))
	for n := range adminNeeds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Needs is what a member op asks of its caller (docs/21 §2): the ops a member runs on
// its own rows (its new tailnet address, the version it serves, the budget of a disk it
// hosts; the server admits them for those rows alone, I-415) need nothing beyond
// membership, every other needs admins.
func (op MemberOp) Needs() Need {
	if op.SelfOnly() {
		return NeedNone
	}
	return NeedAdmins
}

// SelfOnly reports a member op that names the caller's own row, or for set_capacity a
// disk the caller hosts, and no other (I-93, I-415): the server refuses it for any other
// row.
func (op MemberOp) SelfOnly() bool {
	switch op.Op {
	case "update_addr", "set_version", "set_capacity":
		return true
	}
	return false
}

// IsPass reports an op answered only once a pass over the volume, or a copy out of a
// mirror, is done (docs/08, I-189): the client gives it no deadline of its own.
func (op AdminOp) IsPass() bool {
	switch op.Op {
	case "repair_now", "gc_now", "index_now":
		return true
	case "scrape_plan": // a walk of the library that reads its sidecars and gamelists
		return true
	case "versions_revert", "versions_copy": // a file read back from a mirror; a tree restore is a job (docs/16 §6)
		return true
	case "check":
		return op.Scrub
	case "trash_purge": // the whole trash: one proposal after another until it is empty
		return op.All
	}
	return false
}
