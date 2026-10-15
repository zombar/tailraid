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
	// Retention is how long a deleted entry waits in the trash before RunOnce purges it
	// (docs/06 §2, trash.retention); 0 leaves the trash to the user (I-157).
	Retention time.Duration
	log       *slog.Logger
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
	Purged  int `json:"purged"`  // inodes purged from the trash by the retention rule (docs/06 §2)
	Victims int `json:"victims"` // chunk entries removed from metadata
	Deleted int `json:"deleted"` // shard files deleted on live device nodes
	Refused int `json:"refused"` // DELETEs refused by the fence or unreachable (Sweep reconciles)
}

// fsmNow is the leader's clock clamped by the FSM clock (docs/06 §2, docs/05 §7): ages
// are measured on the clock that stamped TombstonedAt and DeletedAt, so a leader whose
// clock lags the proposers never stalls, and Before never goes backwards across leaders.
func (g *GC) fsmNow() int64 { return max(g.clk.Now().UnixNano(), g.st.LastNow()) }

// PurgeTrash purges every deleted root older than before, in batches, as actor, and
// returns how many inodes went (docs/06 §2, I-157). The chunks they held are tombstoned
// at the FSM clock and collected by GCDelete after grace. Actor 0 is the node's own
// retention sweep, proposed as System once users exist (docs/21 §4, I-349); before the
// first user the command carries neither, so it stays readable by a member on a codec
// older than the users' (a stamp is the codec-14 header).
func (g *GC) PurgeTrash(ctx context.Context, before int64, actor meta.UserIdx) (int, error) {
	batch := g.Batch
	if batch == 0 {
		batch = GCBatch
	}
	purged := 0
	cmd := meta.Command{Op: meta.OpPurge}
	if g.st.Enforcing() {
		cmd.Actor, cmd.System = actor, actor == 0
	}
	for g.st.TrashDue(before) > 0 {
		was := g.st.Stats().TrashInodes
		cmd.Purge = &meta.Purge{Before: before, Limit: uint32(batch), Now: g.clk.Now().UnixNano()}
		r, index, err := g.prop.Propose(ctx, cmd)
		if err == nil {
			err = r.Err
		}
		if err != nil {
			return purged, fmt.Errorf("purge: %w", err)
		}
		if err := g.st.WaitApplied(ctx, index); err != nil {
			return purged, err
		}
		n := was - g.st.Stats().TrashInodes
		purged += n
		if n <= 0 {
			return purged, errors.New("purge: no progress") // a root this replica cannot see; never spin
		}
	}
	return purged, nil
}

// RunOnce purges the trash past Retention, then proposes GCDelete{Before: fsmNow − grace}
// and deletes the victims' shards with the fence header. Entries younger than grace stay;
// so do files younger than grace.
func (g *GC) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	if g.Retention > 0 {
		var err error
		if res.Purged, err = g.PurgeTrash(ctx, g.fsmNow()-g.Retention.Nanoseconds(), 0); err != nil {
			return res, fmt.Errorf("gc: %w", err)
		}
	}
	before := g.fsmNow() - g.grace.Nanoseconds()
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
