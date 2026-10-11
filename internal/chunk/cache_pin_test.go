package chunk

import (
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
)

// A pinned entry is never evicted, however much unpinned traffic follows, and the
// budget bounds the unpinned bytes only: with pinned bytes of {0, 1, budget−1, budget, budget+1}
// the unpinned part still fits in budget and every pinned entry stays. An unpin returns the
// entry to the LRU, where the next inserts evict it; a corrupt pinned entry is dropped
// and no longer counts as pinned.
//
// Invariant: I-162
func TestCache_PinnedNeverEvicted(t *testing.T) {
	const budget = 2500
	for _, pinnedBytes := range []int{0, 1, budget - 1, budget, budget + 1} {
		t.Run(itoa(pinnedBytes), func(t *testing.T) {
			fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			c, err := OpenCache(sysfs.OS{}, t.TempDir(), budget, fc)
			if err != nil {
				t.Fatal(err)
			}
			var pinned [][32]byte
			for left, i := pinnedBytes, 0; left > 0; i++ { // in entries of at most 1000 bytes
				n := min(left, 1000)
				p := payload(n, int64(50+i))
				h := sha256.Sum256(p)
				c.Put(h, p)
				if !c.Pin(h) {
					t.Fatal("Pin of a cached entry reports it missing")
				}
				pinned = append(pinned, h)
				left -= n
				fc.Advance(time.Second)
			}
			if got := c.Pinned(); got != int64(pinnedBytes) {
				t.Fatalf("Pinned %d, want %d", got, pinnedBytes)
			}
			// ten chunks of unpinned traffic, four times the budget
			var unpinned [][32]byte
			for i := range 10 {
				fc.Advance(time.Second)
				p := payload(1000, int64(i))
				h := sha256.Sum256(p)
				c.Put(h, p)
				unpinned = append(unpinned, h)
			}
			for _, h := range pinned {
				if !c.Has(h) {
					t.Fatal("pinned entry evicted")
				}
			}
			if c.Used()-c.Pinned() > budget {
				t.Fatalf("unpinned bytes %d over the budget %d", c.Used()-c.Pinned(), budget)
			}
			if c.Has(unpinned[0]) {
				t.Fatal("the oldest unpinned entry survived")
			}
			for _, h := range pinned {
				c.Unpin(h)
			}
			if c.Pinned() != 0 || c.Used() > budget {
				t.Fatalf("after unpin: pinned %d, used %d", c.Pinned(), c.Used())
			}
			for i := range 10 { // more traffic evicts them like any other entry
				fc.Advance(time.Second)
				p := payload(1000, int64(100+i))
				c.Put(sha256.Sum256(p), p)
			}
			for _, h := range pinned {
				if c.Has(h) {
					t.Fatal("an unpinned entry outlived ten budgets of traffic")
				}
			}
		})
	}
	// Pin of a hash the cache lacks says so; a pinned entry that turns out corrupt is
	// dropped on read and leaves the pinned count
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c, err := OpenCache(sysfs.OS{}, t.TempDir(), budget, fc)
	if err != nil {
		t.Fatal(err)
	}
	p := payload(100, 1)
	h := sha256.Sum256(p)
	if c.Pin(h) {
		t.Fatal("Pin of an uncached hash reports it held")
	}
	c.Put(h, p)
	c.Pin(h)
	if err := os.WriteFile(c.path(h), payload(100, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	c.memDropLocked(h) // force the disk read
	if _, ok := c.Get(h); ok {
		t.Fatal("corrupt entry served")
	}
	if c.Pinned() != 0 || c.Has(h) {
		t.Fatalf("corrupt pinned entry still counted: pinned %d, has %v", c.Pinned(), c.Has(h))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
