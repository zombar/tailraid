package chunk

import (
	"context"

	"github.com/zombar/tailraid/internal/meta"
)

// ShardClient moves shards to and from storage nodes, including this node (docs/04 §9,
// docs/10 for S3 nodes). Put is idempotent: a duplicate with identical content refreshes
// the shard's mtime, a duplicate with different content replaces it. Get returns
// ErrNotFound or ErrCorrupt for an unusable shard; Put returns ErrFull when the node
// is out of capacity. Has reports whether the node holds the shard under its final name
// (HEAD): the flusher checks presence, not just liveness, before a dedup hit (L-183).
type ShardClient interface {
	Put(ctx context.Context, node meta.NodeIdx, key ShardKey, body []byte) error
	Get(ctx context.Context, node meta.NodeIdx, key ShardKey) ([]byte, error)
	// GetRange returns the shard's block table and its payload from off, the whole blocks
	// covering n bytes (the caller widens with BlockRange), from a node that serves
	// slices; ErrNoRange from one that does not (docs/04 §5, I-423).
	GetRange(ctx context.Context, node meta.NodeIdx, key ShardKey, off, n int64) (Slice, error)
	Has(ctx context.Context, node meta.NodeIdx, key ShardKey) (bool, error)
	Delete(ctx context.Context, node meta.NodeIdx, key ShardKey) error
}
