package repair

import (
	"context"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
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

// S3Sweeper lists and deletes the shard objects of an S3 node (rpc.Client).
type S3Sweeper interface {
	ListS3(ctx context.Context, node meta.NodeIdx, fn func(rpc.S3Object) error) error
	SweepS3Delete(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) error
}

// SweepS3 is the leader's sweep of one bucket (docs/06 §3): it deletes every shard object
// older than grace that the chunk table does not place on node, and nothing else; it is
// the only DELETE the cluster sends to a bucket (I-138). The caller runs it on the
// leader while Healthy, whose table is current. It returns the number deleted.
func SweepS3(ctx context.Context, b S3Sweeper, st *meta.Store, node meta.NodeIdx, grace time.Duration, now time.Time) (int, error) {
	placed := func(key chunk.ShardKey) bool {
		h, k, m, idx, err := chunk.ParseKey(key)
		if err != nil {
			return true // not a shard: never ours to delete
		}
		c, ok := st.GetChunk(h)
		return ok && c.K == k && c.M == m && idx < len(c.Placement) && c.Placement[idx] == node
	}
	var victims []chunk.ShardKey
	err := b.ListS3(ctx, node, func(o rpc.S3Object) error {
		if now.Sub(o.LastModified) >= grace && !placed(o.Key) { // a younger one may be an upload for a commit to come (I-14)
			victims = append(victims, o.Key)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, k := range victims {
		// a bucket has no conditional delete: placement is checked again at the last
		// moment, and an upload that still slips in leaves a slot the repairer refills
		if placed(k) {
			continue
		}
		if err := b.SweepS3Delete(ctx, node, k); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}
