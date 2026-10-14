package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// ctxGate holds every shard GET until release, and gives one up when its context ends,
// as a real GET on a slow link does.
type ctxGate struct {
	chunk.ShardClient
	entered, cut atomic.Int32
	release      chan struct{}
}

func (g *ctxGate) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	g.entered.Add(1)
	select {
	case <-g.release:
		return g.ShardClient.Get(ctx, node, key)
	case <-ctx.Done():
		g.cut.Add(1)
		return nil, ctx.Err()
	}
}

// A client READ that is abandoned (its request cancelled: the player seeked away or
// closed) cuts the fetch it started once no other READ waits on it, so the chunk nobody
// wants stops taking the link from the one the client asks for next; a fetch another
// READ still waits on goes on and serves it.
// Invariant: I-314
func TestReader_AbandonedReadCutsItsFetch(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(8*cs, 41)
	in := f.Inode(writeAndFlush(t, f, "v", data))
	g := &ctxGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	t.Cleanup(rd.Close) // before the fabric drains its cache: a fetch may still be caching its chunk
	drained(t, rd)
	k := 2

	read := func(ctx context.Context, idx int) chan error {
		errs := make(chan error, 1)
		go func() {
			buf := make([]byte, 4096)
			_, err := rd.ReadAtEarly(ctx, in, buf, int64(idx*cs)+7)
			if err == nil && !bytes.Equal(buf, data[idx*cs+7:idx*cs+7+4096]) {
				err = errors.New("bytes differ")
			}
			errs <- err
		}()
		return errs
	}

	// alone: abandoning the READ cuts its fetch, and the chunk is not cached
	rctx, cancel := context.WithCancel(context.Background())
	errs := read(rctx, 3)
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) >= k }, "the fetch's GETs in flight")
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned read: %v", err)
	}
	testharness.Eventually(t, func() bool { return rd.Fetching() == 0 && g.cut.Load() > 0 }, "the abandoned fetch cut while its GETs hang")
	if cached(f, in.Chunks[3]) {
		t.Fatal("an abandoned fetch cached its chunk")
	}

	// shared: one READ leaves, the other still waits, so the fetch goes on and serves it
	cut := g.cut.Load()
	rctx, cancel = context.WithCancel(context.Background())
	gone := read(rctx, 5)
	testharness.Eventually(t, func() bool { return rd.Fetching() == 1 }, "the fetch started")
	stays := read(context.Background(), 5)
	testharness.Eventually(t, func() bool { return rd.FetchWaiters(in.Chunks[5]) == 2 }, "both READs wait on the fetch")
	cancel()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned read: %v", err)
	}
	if g.cut.Load() != cut {
		t.Fatal("a fetch another READ waits on was cut")
	}
	close(g.release)
	if err := <-stays; err != nil {
		t.Fatalf("the read that stayed: %v", err)
	}
}
