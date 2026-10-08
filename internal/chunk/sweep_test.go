package chunk

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/sysfs"
)

type placement map[ShardKey]bool // key → placed here; absent → unknown

func (p placement) fn(h [32]byte, k, m uint8, idx int) (placed, known bool) {
	v, ok := p[Key(h, k, m, idx)]
	return v, ok
}

func putAged(t *testing.T, s *Store, root string, seed int64, idx int, age time.Duration, now time.Time) ShardKey {
	t.Helper()
	p := payload(500, seed)
	k := keyFor(p, idx)
	if _, err := s.Put(k, p); err != nil {
		t.Fatal(err)
	}
	mt := now.Add(-age)
	if err := os.Chtimes(shardPath(root, k), mt, mt); err != nil {
		t.Fatal(err)
	}
	return k
}

const grace = 10 * time.Minute

// Invariant: I-14
// Lifecycle: L-145
func TestSweep_YoungShardKept(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	now := time.Now()
	young := putAged(t, s, root, 1, 0, grace-time.Minute, now)
	n, err := Sweep(context.Background(), s, grace, now, placement{}.fn) // unknown everywhere
	if err != nil || n != 0 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	if !s.Has(young) {
		t.Fatal("young orphan swept")
	}
}

// Lifecycle: L-135
func TestSweep_OldUnknownDeleted(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	now := time.Now()
	old := putAged(t, s, root, 2, 0, grace+time.Minute, now)
	n, err := Sweep(context.Background(), s, grace, now, placement{}.fn)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	if s.Has(old) {
		t.Fatal("old orphan survived")
	}
	if s.Used() != 0 {
		t.Fatalf("Used = %d", s.Used())
	}
}

// Lifecycle: L-133
func TestSweep_OldNotPlacedHereDeleted(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	now := time.Now()
	k := putAged(t, s, root, 3, 1, grace+time.Hour, now)
	n, err := Sweep(context.Background(), s, grace, now, placement{k: false}.fn)
	if err != nil || n != 1 || s.Has(k) {
		t.Fatalf("relocated shard not swept: n=%d err=%v has=%v", n, err, s.Has(k))
	}
}

// Invariant: I-13
// Lifecycle: L-158, L-164
func TestSweep_OldPlacedHereKept(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	now := time.Now()
	k := putAged(t, s, root, 4, 2, 365*24*time.Hour, now)
	n, err := Sweep(context.Background(), s, grace, now, placement{k: true}.fn)
	if err != nil || n != 0 || !s.Has(k) {
		t.Fatalf("placed shard swept: n=%d err=%v has=%v", n, err, s.Has(k))
	}
}

func TestSweep_StopsOnCancel(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	now := time.Now()
	for i := 0; i < 5; i++ {
		putAged(t, s, root, int64(10+i), 0, grace+time.Hour, now)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := Sweep(ctx, s, grace, now, placement{}.fn)
	if err == nil || n != 0 {
		t.Fatalf("cancelled sweep: n=%d err=%v", n, err)
	}
}
