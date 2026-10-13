package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// cutVolume is a Source whose context is cancelled once after reads chunk reads: the
// node restarting under a sync.
type cutVolume struct {
	*fakeVolume
	after  int64
	n      atomic.Int64
	cancel context.CancelFunc
}

func (c *cutVolume) Chunk(ctx context.Context, h meta.Hash) ([]byte, error) {
	if c.n.Add(1) > c.after {
		c.cancel()
		return nil, context.Canceled
	}
	return c.fakeVolume.Chunk(ctx, h)
}

// A sync cut short leaves what it wrote on the mirror: the host's reconcile sweeps only
// when a snapshot is being deleted, and never while a sync writes, so the retry begun on
// the next tick skips every object the cut sync stored and reads only the rest.
// Invariant: I-243
func TestMirror_InterruptedSyncResumes(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.put(1, "old", 9, bytes.Repeat([]byte{200}, testCS))
	snap(t, m, v, 1) // a complete snapshot the thinning will delete during the retry
	v.dirs[1] = nil
	const files = 40
	for i := range files {
		v.put(1, fmt.Sprintf("f%02d", i), uint64(10+i), bytes.Repeat([]byte{byte(i + 1)}, testCS))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cut := &cutVolume{fakeVolume: v, after: files / 2, cancel: cancel}
	if _, err := m.WriteSnapshot(ctx, cut, 1, Manifest{ID: 2, Name: "s2", Index: 20, ChunkSize: testCS}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cut sync: %v, want context.Canceled", err)
	}
	stored := func() (n int) {
		for i := range files {
			if m.HasObject(meta.Hash(sha256.Sum256(bytes.Repeat([]byte{byte(i + 1)}, testCS)))) {
				n++
			}
		}
		return n
	}
	kept := stored()
	if kept == 0 {
		t.Fatal("the cut sync stored nothing: the test cuts too early")
	}
	// the next tick: the lost sync's row is gone, nothing is deleting
	if _, _, freed, err := m.Reconcile([]Row{{ID: 1}}); err != nil || freed != 0 {
		t.Fatalf("reconcile after the cut: freed %d, %v", freed, err)
	}
	if got := stored(); got != kept {
		t.Fatalf("the reconcile took %d of the %d objects the cut sync stored", kept-got, kept)
	}
	// the retry; the thinning marks snapshot 1 Deleting while it runs
	v.reads.Store(0)
	var during []uint32
	v.onDir = func(uint64) {
		removed, _, _, err := m.Reconcile([]Row{{ID: 1, Deleting: true}})
		if err != nil {
			t.Error(err)
		}
		during = removed
	}
	snap(t, m, v, 3)
	v.onDir = nil
	if want := int64(files - kept); v.reads.Load() != want {
		t.Fatalf("the retry read %d chunks, want %d (the %d stored are skipped)", v.reads.Load(), want, kept)
	}
	if len(during) != 0 {
		t.Fatalf("a deletion completed while a sync wrote: %v", during)
	}
	complete(t, m)
	// once the sync is done the deletion sweeps
	removed, _, freed, err := m.Reconcile([]Row{{ID: 1, Deleting: true}, {ID: 3}})
	if err != nil || !slices.Equal(removed, []uint32{1}) || freed == 0 {
		t.Fatalf("reconcile after the retry: %v freed %d %v", removed, freed, err)
	}
	complete(t, m)
}
