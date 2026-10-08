package repair

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
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
	log    *slog.Logger
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
func (g *GC) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	before := g.clk.Now().UnixNano() - g.grace.Nanoseconds()
	r, index, err := g.prop.Propose(ctx, meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: before}})
	if err == nil {
		err = r.Err
	}
	if err != nil {
		return res, fmt.Errorf("gc: %w", err)
	}
	if err := g.st.WaitApplied(ctx, index); err != nil {
		return res, err
	}
	res.Victims = len(r.Victims)
	nodes := map[meta.NodeIdx]meta.Node{}
	all := g.st.Nodes()
	for i := range all {
		nodes[all[i].Idx] = all[i]
	}
	for _, v := range r.Victims {
		g.Events.Record(v.Hash, meta.EvCollected, fmt.Sprintf("gc index %d", index))
		for i, n := range v.Placement {
			if n == 0 || nodes[n].Kind != meta.KindDevice || !g.live.IsLive(n) {
				continue
			}
			if err := g.shards.DeleteFenced(ctx, n, chunk.Key(v.Hash, v.K, v.M, i), index); err != nil {
				res.Refused++
				continue
			}
			res.Deleted++
		}
	}
	return res, nil
}
