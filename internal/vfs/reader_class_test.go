package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// failGate is a shard client on which every GET from one node blocks until released and
// then fails, or until the reader cancels it.
type failGate struct {
	chunk.ShardClient
	node    meta.NodeIdx
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

var errFailGate = errors.New("failGate: holder failed")

func (g *failGate) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	if node != g.node {
		return g.ShardClient.Get(ctx, node, key)
	}
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return nil, errFailGate
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A hedged GET skips a relayed holder while k direct holders are live, and asks it once
// a direct one failed; with fewer than k direct holders the relayed ones serve at once.
// The relayed holder ranks last among the direct ones' equals.
// Invariant: I-327
func TestReader_HedgeSkipsRelayedHolders_Table(t *testing.T) {
	const k = 2
	cases := []struct {
		name    string
		relayed []meta.NodeIdx // of the three holders 1 (self), 2 and 3
		direct  int            // live direct holders
	}{
		{"all direct", nil, 3},
		{"one relayed", []meta.NodeIdx{3}, 2},
		{"fewer than k direct", []meta.NodeIdx{2, 3}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, k, 1)
			for _, n := range tc.relayed {
				f.SetClass(n, meta.PathRelay)
			}
			data := testharness.Pattern(int(testharness.ChunkSize), 9)
			ino := f.Create("a")
			must(t, f.Ov.Write(ino, 0, 0, data))
			must(t, f.Flusher().FlushInode(ctx, ino))
			in := f.Inode(ino)
			c, _ := f.FSM.GetChunk(in.Chunks[0])
			order := f.Rank(c.Placement)
			for i, n := range order[len(order)-len(tc.relayed):] {
				if f.Class(n) != meta.PathRelay {
					t.Fatalf("rank %v: holder %d at %d is not relayed", order, n, i)
				}
			}
			victim := order[0] // the holder asked first, always direct here
			g := &failGate{ShardClient: f, node: victim, release: make(chan struct{}), entered: make(chan struct{})}
			rd := vfs.NewReader(f.FSM, f, g, nil, nil)
			gets := func(n meta.NodeIdx) int { return f.Gets(n) }
			before := map[meta.NodeIdx]int{}
			for n := meta.NodeIdx(1); n <= 3; n++ {
				before[n] = gets(n)
			}
			type res struct {
				b   []byte
				err error
			}
			done := make(chan res, 1)
			go func() {
				b, err := rd.ReadChunk(ctx, in.Chunks[0])
				done <- res{b, err}
			}()
			<-g.entered
			if tc.direct >= k && len(tc.relayed) > 0 {
				// three hedge delays pass: the relayed holder is not asked while the two
				// direct holders are live, so the read waits on the victim
				until := time.Now().Add(3 * 200 * time.Millisecond)
				testharness.Eventually(t, func() bool { return time.Now().After(until) }, "clock")
				select {
				case r := <-done:
					t.Fatalf("the read completed (%v) without the victim: it hedged to the relayed holder", r.err)
				default:
				}
				for _, n := range tc.relayed {
					if gets(n)-before[n] != 0 {
						t.Fatalf("holder %d (relayed) was asked while %d direct holders were live", n, tc.direct)
					}
				}
				close(g.release) // the victim fails: fewer than k direct holders now
			}
			var r res
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the read did not complete")
			}
			must(t, r.err)
			if !bytes.Equal(r.b, data) {
				t.Fatalf("differs at %d", testharness.FirstDiff(r.b, data))
			}
			for _, n := range tc.relayed {
				if gets(n)-before[n] != 1 {
					t.Fatalf("relayed holder %d served %d GETs, want 1 (victim %d, rank %v)", n, gets(n)-before[n], victim, order)
				}
			}
		})
	}
}
