package chunk

import (
	"context"

	"github.com/gurgeh/tailraid/internal/meta"
)

// ShardClient moves shards to and from storage nodes, including this node (docs/04 §9,
// docs/10 for S3 nodes). Put is idempotent: a duplicate with identical content refreshes
// the shard's mtime, a duplicate with different content replaces it. Get returns
// ErrNotFound or ErrCorrupt for an unusable shard; Put returns ErrFull when the node
// is out of capacity.
type ShardClient interface {
	Put(ctx context.Context, node meta.NodeIdx, key ShardKey, body []byte) error
	Get(ctx context.Context, node meta.NodeIdx, key ShardKey) ([]byte, error)
	Delete(ctx context.Context, node meta.NodeIdx, key ShardKey) error
}
