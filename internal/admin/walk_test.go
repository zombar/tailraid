package admin_test

import (
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// countingLive answers from a fixed view and counts every question by node.
type countingLive struct {
	mu   sync.Mutex
	dead map[meta.NodeIdx]bool
	asks map[meta.NodeIdx]int
}

func (c *countingLive) IsLive(idx meta.NodeIdx) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks[idx]++
	return !c.dead[idx]
}

func (c *countingLive) DeadFor(idx meta.NodeIdx) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks[idx]++
	if c.dead[idx] {
		return time.Hour
	}
	return 0
}

// One status walk asks the liveness source about each member at most once (IsLive and
// DeadFor each), however many slots it judges, and judges every slot by those answers:
// asking per slot made `walk_ms` grow with the volume, 1.1 s every 2 s at 115k chunks on
// a real node (docs/12 §8).
// Invariant: I-375
func TestScanHealth_AsksEachMemberOnce(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	for i, name := range []string{"a", "b", "c", "d"} {
		ino := f.Create(name)
		must(t, f.Ov.Write(ino, 4096, 4096, testharness.Pattern(2*int(testharness.ChunkSize), byte(i+1))))
		must(t, f.Flusher().FlushInode(ctx, ino))
	}
	live := &countingLive{dead: map[meta.NodeIdx]bool{5: true}, asks: map[meta.NodeIdx]int{}}
	v := admin.ScanHealth(f.FSM, live, 10*time.Minute)
	slots := 0
	for _, n := range v.Slots {
		slots += n
	}
	if slots < 12 {
		t.Fatalf("walk saw %d slots, want at least 12: %+v", slots, v.Slots)
	}
	for idx, n := range live.asks {
		if n > 2 {
			t.Errorf("member %d asked %d times in one walk, want at most 2 (IsLive, DeadFor)", idx, n)
		}
	}
	if v.Missing[5] != v.Slots[5] || v.Slots[5] == 0 {
		t.Fatalf("dead member's slots: missing %d of %d", v.Missing[5], v.Slots[5])
	}
	if v.RepairPending == 0 {
		t.Fatalf("slots on a member dead past fill_after not pending: %+v", v)
	}
}
