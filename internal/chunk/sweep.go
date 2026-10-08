package chunk

import (
	"context"
	"fmt"
	"time"
)

// Sweep deletes every shard whose mtime is older than grace and that the replicated chunk
// table either does not know (orphan of a flush that never committed) or does not place
// on this node (relocated by repair). placedHere answers from the local FSM copy only.
// Shards younger than grace are never touched (I-14): they may belong to an in-flight
// flush whose Commit has not yet been applied locally.
func Sweep(ctx context.Context, s *Store, grace time.Duration, now time.Time,
	placedHere func(hash [32]byte, k, m uint8, idx int) (placed, known bool)) (deleted int, err error) {
	cutoff := now.Add(-grace)
	var victims []ShardKey
	err = s.Walk(func(k ShardKey, _ int64, mtime time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !mtime.Before(cutoff) {
			return nil
		}
		h, kk, mm, idx, err := ParseKey(k)
		if err != nil {
			return nil
		}
		placed, known := placedHere(h, kk, mm, idx)
		if !known || !placed {
			victims = append(victims, k)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("chunk: sweep walk: %w", err)
	}
	for _, k := range victims {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if _, err := s.Delete(k); err != nil {
			return deleted, fmt.Errorf("chunk: sweep delete %s: %w", k, err)
		}
		deleted++
	}
	return deleted, nil
}
