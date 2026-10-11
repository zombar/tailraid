package chunk

import "context"

type backgroundKey struct{}

// Background marks ctx as background work: a shard request made under it tells the
// node serving it that a client-path request may go first (docs/06 §4, I-175). The
// repairer's passes and the capacity rebalance run under the mark; a gateway's reads
// and flushes never do.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx carries the Background mark.
func IsBackground(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}
