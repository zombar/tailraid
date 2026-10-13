package mirror

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// gatedVolume answers a chunk only once want reads are in flight at the same time (or
// after wait, as unreadable): a writer that reads one chunk at a time never gets there.
type gatedVolume struct {
	*fakeVolume
	want int
	wait time.Duration

	mu      sync.Mutex
	cond    *sync.Cond
	in, max int
}

func (g *gatedVolume) Chunk(ctx context.Context, h meta.Hash) ([]byte, error) {
	g.mu.Lock()
	g.in++
	g.max = max(g.max, g.in)
	g.cond.Broadcast()
	timedOut := false
	timer := time.AfterFunc(g.wait, func() {
		g.mu.Lock()
		timedOut = true
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	for g.max < g.want && !timedOut {
		g.cond.Wait()
	}
	timer.Stop()
	reached := g.max >= g.want
	g.in--
	g.mu.Unlock()
	if !reached {
		return nil, errors.New("read alone")
	}
	return g.fakeVolume.Chunk(ctx, h)
}

// A sync reads several chunks at once (docs/16 §3, I-239): a chunk costs a round trip
// to other hosts and a durable write to the mirror disk, and one at a time a 50 GiB
// sync took over an hour. The snapshot it writes is the same complete one: every object
// before the tree naming it, the manifest last (I-180), every chunk present.
// Invariant: I-239
func TestMirror_SyncReadsChunksConcurrently(t *testing.T) {
	v := newVolume()
	v.mkdir(1, "d", 2)
	v.mkdir(2, "e", 3)
	for i := range 6 {
		v.put(uint64(1+i%3), "f"+string(rune('0'+i)), uint64(10+i), bytes.Repeat([]byte{byte(1 + i)}, 2*testCS))
	}
	v.put(3, "dup", 30, bytes.Repeat([]byte{1}, 2*testCS)) // a's chunks again: fetched once
	g := &gatedVolume{fakeVolume: v, want: 4, wait: 2 * time.Second}
	g.cond = sync.NewCond(&g.mu)
	m, _ := newMirror(t, sysfs.OS{})
	man, err := m.WriteSnapshot(context.Background(), g, 1, Manifest{ID: 1, Name: "s", ChunkSize: testCS})
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Missing) != 0 {
		t.Fatalf("%d chunks missing: the sync read them one at a time (at most %d at once)", len(man.Missing), g.max)
	}
	if got := v.reads.Load(); got != 6 {
		t.Fatalf("%d chunk reads, want 6 (each distinct chunk once)", got)
	}
	complete(t, m)
}
