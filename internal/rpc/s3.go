package rpc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
)

// SetS3 wires S3 routing (docs/10 §4.2): of returns the bucket client of an S3 target
// (nil when this device holds no credentials for it) and whether the index is an S3 row.
func (c *Client) SetS3(of func(meta.NodeIdx) (bucket *s3.Bucket, isS3 bool)) { c.s3Of = of }

// ShardObject is the object key of a shard under a bucket's prefix: the layout of a
// disk's shards/ directory (I-52).
func ShardObject(key chunk.ShardKey) string { return "shards/" + string(key[:2]) + "/" + string(key) }

// ErrNoCredentials is an S3 target this device holds no credentials for: it is
// unreachable from here (docs/10 §4.1).
var ErrNoCredentials = errors.New("rpc: no credentials for this S3 node on this device")

// bucket resolves an S3 target. isS3 false means node is not an S3 row.
func (c *Client) bucket(node meta.NodeIdx) (b *s3.Bucket, isS3 bool, err error) {
	if c.s3Of == nil {
		return nil, false, nil
	}
	b, isS3 = c.s3Of(node)
	if isS3 && b == nil {
		return nil, true, fmt.Errorf("%w: %w: node %d", ErrUnavailable, ErrNoCredentials, node)
	}
	return b, isS3, nil
}

// s3Err maps a bucket error onto the shard client's: an absent object is ErrNotFound,
// everything else is a target that cannot be reached.
func s3Err(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, s3.ErrNotFound) {
		return chunk.ErrNotFound
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func s3Put(ctx context.Context, b *s3.Bucket, key chunk.ShardKey, payload []byte) error {
	if _, _, _, _, err := chunk.ParseKey(key); err != nil {
		return err
	}
	return s3Err(b.Put(ctx, ShardObject(key), chunk.Frame(payload, sha256.Sum256(payload))))
}

func s3Get(ctx context.Context, b *s3.Bucket, key chunk.ShardKey) ([]byte, error) {
	if _, _, _, _, err := chunk.ParseKey(key); err != nil {
		return nil, err
	}
	raw, err := b.Get(ctx, ShardObject(key), chunk.HeaderSize+chunk.MaxPayload)
	if err != nil {
		return nil, s3Err(err)
	}
	return chunk.Unframe(raw) // verified on receipt like a disk shard (I-11, I-52)
}

func s3Has(ctx context.Context, b *s3.Bucket, key chunk.ShardKey) (bool, error) {
	if _, _, _, _, err := chunk.ParseKey(key); err != nil {
		return false, err
	}
	has, err := b.Has(ctx, ShardObject(key))
	return has, s3Err(err)
}

// errS3Delete refuses a delete on an S3 target: the disk fence has no counterpart on a
// bucket, so objects leave only through the leader's S3 sweep (docs/10 §4.2, I-138).
var errS3Delete = fmt.Errorf("%w: objects leave a bucket only through the leader's S3 sweep", ErrFenced)

// ProbeS3 checks an S3 target's bucket and credentials from this device (docs/10 §4.3).
func (c *Client) ProbeS3(ctx context.Context, node meta.NodeIdx) error {
	b, isS3, err := c.bucket(node)
	if err != nil {
		return err
	}
	if !isS3 {
		return fmt.Errorf("%w: node %d is not an S3 node", ErrUnavailable, node)
	}
	return b.Probe(ctx)
}

// S3Object is one shard object of a bucket listing.
type S3Object struct {
	Key          chunk.ShardKey
	LastModified time.Time
}

// ListS3 lists the shard objects of an S3 target. Objects whose name is not a shard key
// are not reported, so nothing else in the prefix is ever a sweep candidate.
func (c *Client) ListS3(ctx context.Context, node meta.NodeIdx, fn func(S3Object) error) error {
	b, isS3, err := c.bucket(node)
	if err != nil {
		return err
	}
	if !isS3 {
		return fmt.Errorf("%w: node %d is not an S3 node", ErrUnavailable, node)
	}
	return b.List(ctx, "shards/", func(o s3.Object) error {
		if key, ok := shardOf(o.Key); ok {
			return fn(S3Object{Key: key, LastModified: o.LastModified})
		}
		return nil
	})
}

// shardOf is the shard key an object name holds, if it is a shard's ShardObject.
func shardOf(name string) (chunk.ShardKey, bool) {
	key := chunk.ShardKey(name[strings.LastIndexByte(name, '/')+1:])
	_, _, _, _, err := chunk.ParseKey(key)
	return key, err == nil && name == ShardObject(key)
}

// SweepS3Delete deletes one shard object: the S3 sweep's delete and the only DELETE the
// cluster sends to a bucket (I-138).
func (c *Client) SweepS3Delete(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) error {
	b, isS3, err := c.bucket(node)
	if err != nil {
		return err
	}
	if !isS3 {
		return fmt.Errorf("%w: node %d is not an S3 node", ErrUnavailable, node)
	}
	return b.Delete(ctx, ShardObject(key))
}
