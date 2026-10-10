package soak

import (
	"context"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/s3"
)

// NodeRef identifies a node of the environment.
type NodeRef struct {
	Name string       `json:"name"`
	Idx  meta.NodeIdx `json:"idx"`
	RPC  string       `json:"rpc"`            // host:port as this driver reaches it (informational)
	Host string       `json:"host,omitempty"` // an added disk's host node name (docs/14); "" for a node
}

// DiskInfo is one of a node's disks as its budget sees it (GET /dev/disk, docs/12 §3).
type DiskInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"` // the disk row's name, "<host>/<n>", as status reports it
	Used   uint64 `json:"used"` // shard bytes held
	Free   uint64 `json:"free"`
	Total  uint64 `json:"total"`
	Pinned bool   `json:"pinned"` // its budget is pinned by the config: a fill moves nothing
}

// Caps lists what an Env can do; phases needing more are recorded as skipped.
type Caps struct {
	Kill         bool // Kill/Start/Restart
	Partition    bool
	Corrupt      bool
	MultiGateway bool // Volume available on more than one node
	Disks        bool // FailDisk/RestoreDisk on the hosts' added disks (docs/14)
	S3           bool // Bucket: phase 15 can reach a bucket behind the cluster's back (docs/10 §4)
	AddS3        bool // AddS3: the environment provides a bucket of its own for phase 15 to register
	// Mutate allows the phases that change the cluster itself rather than its files:
	// policy changes, drains and removals, filling a node's disk. A live cluster allows
	// them only with --destructive.
	Mutate bool
}

// Env is one target cluster (docs/09 §6).
type Env interface {
	Name() string // "compose 4 nodes 2+1", "live cluster 0a2abc10"
	Nodes(ctx context.Context) ([]NodeRef, error)
	Volume(node NodeRef) (Volume, bool)
	Caps() Caps
	Wait(ctx context.Context, d time.Duration) error // real time, or the fake clock in tests

	// admin surface (one node's view; leader-only ops are routed by the Env)
	Status(ctx context.Context, node NodeRef) (admin.Report, error)
	RepairNow(ctx context.Context) (repair.Stats, error)
	Scrub(ctx context.Context) (repair.Stats, error)
	GCNow(ctx context.Context) (repair.Result, error)
	// PurgeTrash deletes every entry in the trash for good (`tailraid trash purge --all`,
	// docs/12 §10) and returns how many inodes went; a removed file's chunks are then
	// tombstoned and GC collects them after grace.
	PurgeTrash(ctx context.Context) (int, error)
	SetPolicy(ctx context.Context, k, m uint8) error
	SetState(ctx context.Context, node NodeRef, state meta.NodeState) error
	// Remove takes a member out of the node table (`node remove`, no --force).
	Remove(ctx context.Context, node NodeRef) error
	// Fsck runs the node's local check: ok is "no violation"; orphans counts the shards
	// the chunk table does not place on that node (swept after grace, not a violation).
	Fsck(ctx context.Context, node NodeRef) (ok bool, orphans int, detail string, err error)
	Chunks(ctx context.Context, node NodeRef, path string) ([]admin.ChunkReport, error)
	FSMHash(ctx context.Context, node NodeRef) (applied uint64, hash string, err error)
	// Goroutines is the node's goroutine dump (/dev/goroutines), taken after a failed
	// phase so a hang is diagnosed from the report rather than by killing the node.
	Goroutines(ctx context.Context, node NodeRef) (string, error)
	// automation (docs/01 §Node budget, docs/03): the node's disks as their budgets see
	// them, a scratch file that consumes one disk (bytes 0 removes it; /dev/fill), auto
	// redundancy.
	Disks(ctx context.Context, node NodeRef) ([]DiskInfo, error)
	Fill(ctx context.Context, node NodeRef, disk string, bytes uint64) error
	SetPolicyAuto(ctx context.Context) error

	// faults
	Kill(ctx context.Context, node NodeRef) error
	Start(ctx context.Context, node NodeRef) error
	Restart(ctx context.Context, nodes []NodeRef) error
	Partition(ctx context.Context, a, b []NodeRef) error
	Heal(ctx context.Context) error
	Corrupt(ctx context.Context, node NodeRef, key string) error
	// FailDisk closes an added disk as an unmounted one would (its marker gone, its host
	// restarted); RestoreDisk brings it back. disk names its host (NodeRef.Host).
	FailDisk(ctx context.Context, disk NodeRef) error
	RestoreDisk(ctx context.Context, disk NodeRef) error
	// AddDisk gives host one more added disk (`node add-disk` on a spare mount).
	AddDisk(ctx context.Context, host NodeRef) error

	// S3 nodes (docs/10 §4): AddS3 registers the environment's bucket as S3 node name
	// (`node add-s3` on one node, `node keys` on the others); Bucket is that bucket as
	// the driver reaches it, under the prefix the nodes use, for looking behind the
	// cluster's back. A live cluster offers only Bucket, for the S3 node it already has.
	AddS3(ctx context.Context, name string) error
	Bucket(ctx context.Context) (*s3.Bucket, error)
}
