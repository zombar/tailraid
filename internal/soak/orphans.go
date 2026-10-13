package soak

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// orphan is one shard key fsck listed as unplaced, with what explains it ("" if nothing).
type orphan struct {
	Key    string
	Reason string
}

// orphanReason is why a shard of a chunk may sit unplaced on a disk, read from the chunk's
// history merged from every node's event ring (L-190, docs/06 §2): "" when nothing
// recorded explains it.
func orphanReason(events []meta.ChunkEvent) string {
	uploaded, committed := 0, 0
	var reason string
	for _, e := range events {
		switch e.Kind {
		case meta.EvPutFailed:
			reason = "a PUT failed or was cut and landed anyway"
		case meta.EvCollected:
			reason = "collected by GC"
		case meta.EvRepaired, meta.EvPartial, meta.EvMigrated:
			reason = "a slot moved by repair"
		case meta.EvFill:
			reason = "re-uploaded by a fill"
		case meta.EvUploaded:
			uploaded++
		case meta.EvCommitted:
			committed++
		default:
		}
	}
	if reason == "" && uploaded > committed {
		reason = "uploaded for a commit that did not apply"
	}
	return reason
}

// judgeNewOrphans sorts the keys in after but not in before by whether their chunk's
// history explains them; a key that is not a shard is never explained.
func judgeNewOrphans(before, after []string, history func(hash string) []meta.ChunkEvent) (explained, unexplained []orphan) {
	for _, k := range after {
		if slices.Contains(before, k) {
			continue
		}
		o := orphan{Key: k}
		if h, _, _, _, err := chunk.ParseKey(chunk.ShardKey(k)); err == nil {
			o.Reason = orphanReason(history(hex.EncodeToString(h[:])))
		}
		if o.Reason == "" {
			unexplained = append(unexplained, o)
		} else {
			explained = append(explained, o)
		}
	}
	return explained, unexplained
}

// chunkHistory merges a chunk's events from every node's ring (the flusher records on the
// writing gateway, GC and repair on the leader).
func (r *Run) chunkHistory(ctx context.Context) func(hash string) []meta.ChunkEvent {
	return func(hash string) []meta.ChunkEvent {
		var out []meta.ChunkEvent
		for _, n := range r.nodes {
			reps, err := r.Env.Chunks(ctx, n, hash)
			if err != nil {
				continue
			}
			for i := range reps {
				out = append(out, reps[i].Events...)
			}
		}
		return out
	}
}

// orphanText is a short account of judged orphans for a check's detail.
func orphanText(os []orphan, limit int) string {
	var b strings.Builder
	for i, o := range os {
		if i == limit {
			fmt.Fprintf(&b, "; and %d more", len(os)-limit)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(o.Key)
		if o.Reason != "" {
			b.WriteString(" (" + o.Reason + ")")
		}
	}
	return b.String()
}
