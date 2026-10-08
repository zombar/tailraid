package soak

import (
	"context"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
)

// NodeRef identifies a node of the environment.
type NodeRef struct {
	Name string       `json:"name"`
	Idx  meta.NodeIdx `json:"idx"`
	RPC  string       `json:"rpc"` // host:port as this driver reaches it (informational)
}

// Caps lists what an Env can do; phases needing more are recorded as skipped.
type Caps struct {
	Kill         bool // Kill/Start/Restart
	Partition    bool
	Corrupt      bool
	MultiGateway bool // Volume available on more than one node
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
	SetPolicy(ctx context.Context, k, m uint8) error
	SetState(ctx context.Context, node NodeRef, state meta.NodeState) error
	Fsck(ctx context.Context, node NodeRef) (ok bool, detail string, err error)
	Chunks(ctx context.Context, node NodeRef, path string) ([]admin.ChunkReport, error)
	FSMHash(ctx context.Context, node NodeRef) (applied uint64, hash string, err error)

	// faults
	Kill(ctx context.Context, node NodeRef) error
	Start(ctx context.Context, node NodeRef) error
	Restart(ctx context.Context, nodes []NodeRef) error
	Partition(ctx context.Context, a, b []NodeRef) error
	Heal(ctx context.Context) error
	Corrupt(ctx context.Context, node NodeRef, key string) error
}
