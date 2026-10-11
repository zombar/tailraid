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

// refuseBackground holds every background GET until release and then refuses it, as a
// busy holder does under load (I-175); a client's GETs pass straight through.
type refuseBackground struct {
	chunk.ShardClient
	entered atomic.Int32
	release chan struct{}
}

func (g *refuseBackground) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	if !chunk.IsBackground(ctx) {
		return g.ShardClient.Get(ctx, node, key)
	}
	g.entered.Add(1)
	<-g.release
	return nil, errors.New("background GET refused: holder busy")
}

// A READ that joined a prefetch which then failed at background priority fetches the
// chunk for itself rather than returning the prefetch's error: the prefetch's failure
// says nothing about a client read (CI: phase 12 read EIO through another gateway).
// Regression: I-203
func TestReader_JoinedPrefetchFailure_ReadFetchesItself(t *testing.T) {
	for _, early := range []bool{false, true} {
		f := testharness.NewFabric(t, 4, 2, 1)
		cs := int(testharness.ChunkSize)
		data := testharness.Pattern(16*cs, 31)
		in := f.Inode(writeAndFlush(t, f, "a", data))
		g := &refuseBackground{ShardClient: f, release: make(chan struct{})}
		rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
		drained(t, rd)
		rd.Prefetch(in, 0)
		testharness.Eventually(t, func() bool { return g.entered.Load() > 0 }, "prefetches in flight")
		got := make([]byte, cs)
		errs := make(chan error, 1)
		go func() {
			if early {
				_, err := rd.ReadAtEarly(ctx, in, got, int64(cs))
				errs <- err
				return
			}
			b, err := rd.ReadChunk(ctx, in.Chunks[1])
			copy(got, b)
			errs <- err
		}()
		testharness.Eventually(t, func() bool { _, joined := rd.ReadaheadStats(); return joined == 1 }, "the client joined chunk 1's prefetch")
		close(g.release)
		if err := <-errs; err != nil {
			t.Fatalf("early=%v: the joined read failed with the prefetch: %v", early, err)
		}
		if !bytes.Equal(got, data[cs:2*cs]) {
			t.Fatalf("early=%v: joined read differs at %d", early, testharness.FirstDiff(got, data[cs:2*cs]))
		}
		testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	}
}
