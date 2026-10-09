package repair

import (
	"context"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// SweepLocal removes this node's orphan shards (docs/06 §3). It only runs when the node
// is Healthy and has applied the log at least as far as the leader had committed when
// the scan began; otherwise it returns 0 and nil, since a stale chunk table would call
// committed shards orphans (I-13). applied and leaderApplied are Raft indexes.
func SweepLocal(ctx context.Context, store *chunk.Store, st *meta.Store, self meta.NodeIdx, grace time.Duration, clk clock.Clock,
	healthy func() bool, applied func() uint64, leaderApplied func() (uint64, bool),
) (int, error) {
	if !healthy() {
		return 0, nil
	}
	want, ok := leaderApplied()
	if !ok || applied() < want {
		return 0, nil
	}
	placedHere := func(h [32]byte, k, m uint8, idx int) (bool, bool) {
		c, ok := st.GetChunk(h)
		if !ok {
			return false, false
		}
		if c.K != k || c.M != m || idx >= len(c.Placement) {
			return false, true
		}
		return c.Placement[idx] == self, true
	}
	return chunk.Sweep(ctx, store, grace, clk.Now(), placedHere)
}
