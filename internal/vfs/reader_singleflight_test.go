package vfs_test

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// gate is a shard client whose GETs wait until the test releases them, so concurrent
// readers of one chunk overlap deterministically.
type gate struct {
	chunk.ShardClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gate) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.ShardClient.Get(ctx, node, key)
}

// Concurrent reads of the same chunk (readahead, several clients) share one fetch: k
// shard GETs in total, not k per reader, and every reader gets the bytes.
func TestReader_ConcurrentReadsOfOneChunk_FetchOnce(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(cs, 5)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	g := &gate{ShardClient: f, entered: make(chan struct{}), release: make(chan struct{})}
	// with the fabric's cache: a reader that reaches the chunk only after the shared fetch
	// completed (the goroutines start in any order) is served from it, so the GET count
	// is exact whatever the scheduling
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	before := 0
	for n := meta.NodeIdx(1); n <= 3; n++ {
		before += f.Gets(n)
	}
	const readers = 8
	results := make(chan []byte, readers)
	errs := make(chan error, readers)
	for range readers {
		go func() {
			b, err := rd.ReadChunk(ctx, in.Chunks[0])
			results <- b
			errs <- err
		}()
	}
	<-g.entered // one fetch is in flight; the others must be waiting on it, not fetching
	close(g.release)
	for i := range readers {
		must(t, <-errs)
		if got := <-results; !bytes.Equal(got, data) {
			t.Fatalf("reader %d: differs at %d", i, testharness.FirstDiff(got, data))
		}
	}
	gets := -before
	for n := meta.NodeIdx(1); n <= 3; n++ {
		gets += f.Gets(n)
	}
	if gets != 2 {
		t.Fatalf("%d shard GETs for %d concurrent readers, want k=2", gets, readers)
	}
}
