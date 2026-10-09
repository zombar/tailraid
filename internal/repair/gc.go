package repair

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// GC is the leader-only tombstone collector (docs/06 §2).
type GC struct {
	Events *meta.EventRing // chunk history (nil unless dev.enabled)
	st     *meta.Store
	prop   meta.Proposer
	shards Shards
	live   Liveness
	clk    clock.Clock
	grace  time.Duration
	// Batch is the number of chunk entries one GCDelete removes; 0 is GCBatch.
	Batch int
	log   *slog.Logger
}

// NewGC wires the collector.
func NewGC(st *meta.Store, prop meta.Proposer, shards Shards, live Liveness, clk clock.Clock, grace time.Duration, log *slog.Logger) *GC {
	if log == nil {
		log = slog.Default()
	}
	return &GC{st: st, prop: prop, shards: shards, live: live, clk: clk, grace: grace, log: log}
}

// Result of one GC pass.
type Result struct {
	Victims int `json:"victims"` // chunk entries removed from metadata
	Deleted int `json:"deleted"` // shard files deleted on live device nodes
	Refused int `json:"refused"` // DELETEs refused by the fence or unreachable (Sweep reconciles)
}

// RunOnce proposes GCDelete{Before: fsmNow − grace} and deletes the victims' shards with
// the fence header. Entries younger than grace stay; so do files younger than grace.
// fsmNow is the leader's clock clamped by the FSM clock (docs/06 §2, docs/05 §7): grace is
// measured on the same clock that stamped TombstonedAt, so a leader whose clock lags the
// proposers never stalls GC and Before never goes backwards across leader changes.
func (g *GC) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	before := max(g.clk.Now().UnixNano(), g.st.LastNow()) - g.grace.Nanoseconds()
	batch := g.Batch
	if batch == 0 {
		batch = GCBatch
	}
	// one entry per batch (I-103): an rm -rf of terabytes is many short applies, not one
	// long stall under the FSM lock and one huge result
	for {
		r, index, err := g.prop.Propose(ctx, meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: before, Limit: uint32(batch)}})
		if err == nil {
			err = r.Err
		}
		if err != nil {
			return res, fmt.Errorf("gc: %w", err)
		}
		if err := g.st.WaitApplied(ctx, index); err != nil {
			return res, err
		}
		res.Victims += len(r.Victims)
		g.deleteShards(ctx, r.Victims, index, &res)
		if len(r.Victims) < batch {
			return res, nil
		}
	}
}

// GCBatch is the default number of chunk entries one GCDelete removes (GC.Batch).
const GCBatch = 1024

// deleteShards deletes the victims' shards on every live target with the fence header.
func (g *GC) deleteShards(ctx context.Context, victims []meta.GCVictim, index uint64, res *Result) {
	nodes := map[meta.NodeIdx]meta.Node{}
	all := g.st.Nodes()
	for i := range all {
		nodes[all[i].Idx] = all[i]
	}
	for _, v := range victims {
		g.Events.Record(v.Hash, meta.EvCollected, fmt.Sprintf("gc index %d", index))
		for i, n := range v.Placement {
			if n == 0 || nodes[n].Kind != meta.KindDisk || !g.live.IsLive(n) { // only disks hold shards (docs/14 §9); S3 is deferred (docs/10)
				continue
			}
			err := g.shards.DeleteFenced(ctx, n, chunk.Key(v.Hash, v.K, v.M, i), index)
			if errors.Is(err, rpc.ErrFenced) {
				// A follower still applying the GC entry: once more shortly. This is a
				// sub-second real-time backoff for a remote process, not a scheduled event,
				// so it deliberately bypasses the clock seam (a fake clock nobody advances
				// would hang here).
				select {
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
				}
				err = g.shards.DeleteFenced(ctx, n, chunk.Key(v.Hash, v.K, v.M, i), index)
			}
			if err != nil {
				res.Refused++
				continue
			}
			res.Deleted++
		}
	}
}
