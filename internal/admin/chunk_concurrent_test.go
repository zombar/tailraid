package admin_test

import (
	"context"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// gate is a ShardChecker that holds every HEAD until the test releases it and records
// how many were in flight at once.
type gate struct {
	inner          admin.ShardChecker
	release        chan struct{}
	mu             sync.Mutex
	inflight, peak int
}

func (g *gate) Has(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) (bool, error) {
	g.mu.Lock()
	g.inflight++
	g.peak = max(g.peak, g.inflight)
	g.mu.Unlock()
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
	return g.inner.Has(ctx, node, key)
}

func (g *gate) count() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight, g.peak
}

// A page's shard checks run concurrently, PageWorkers at a time and no more, and the
// reports come back in the file's chunk order with every slot verified.
//
// Invariant: I-161
func TestDescribeChunksPage_Concurrent(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	const n = 40
	c.WriteFile(0, "/f", testharness.Pattern(n*int(testharness.ChunkSize), 7))
	c.Settle()
	n0 := c.Nodes[0]
	g := &gate{inner: n0.Client, release: make(chan struct{})}
	type result struct {
		total int
		reps  []admin.ChunkReport
		err   error
	}
	done := make(chan result, 1)
	go func() {
		total, reps, err := admin.DescribeChunksPage(context.Background(), n0.Node.FSM, n0.Prober, g, nil, "/f", 0, 64)
		done <- result{total, reps, err}
	}()
	testharness.Eventually(t, func() bool { in, _ := g.count(); return in == admin.PageWorkers }, "the page never had PageWorkers checks in flight")
	close(g.release)
	res := <-done
	if res.err != nil || res.total != n || len(res.reps) != n {
		t.Fatalf("page: %v, %d of %d", res.err, len(res.reps), res.total)
	}
	if _, peak := g.count(); peak != admin.PageWorkers {
		t.Fatalf("peak %d checks in flight, want exactly %d", peak, admin.PageWorkers)
	}
	for i, rep := range res.reps {
		if rep.Hash != hashHex(t, n0, i) {
			t.Fatalf("report %d is chunk %s, out of order", i, rep.Hash[:8])
		}
		if rep.Slots == nil || len(rep.Slots) != 3 {
			t.Fatalf("report %d slots %+v", i, rep.Slots)
		}
		for _, s := range rep.Slots {
			if s.Present == nil || !*s.Present {
				t.Fatalf("report %d slot %+v not verified", i, s)
			}
		}
	}
}
