package chunk

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
)

func TestCache_PutGetAndMiss(t *testing.T) {
	c, err := OpenCache(sysfs.OS{}, t.TempDir(), 1<<20, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	p := payload(1000, 1)
	h := sha256.Sum256(p)
	if _, ok := c.Get(h); ok {
		t.Fatal("hit before put")
	}
	c.Put(h, p)
	got, ok := c.Get(h)
	if !ok || !bytes.Equal(got, p) {
		t.Fatal("miss after put")
	}
	if c.Used() != 1000 {
		t.Fatalf("Used = %d", c.Used())
	}
}

func TestCache_LRUEvictsBySize(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c, err := OpenCache(sysfs.OS{}, t.TempDir(), 2500, fc)
	if err != nil {
		t.Fatal(err)
	}
	var hs [][32]byte
	for i := 0; i < 3; i++ {
		p := payload(1000, int64(i))
		h := sha256.Sum256(p)
		hs = append(hs, h)
		c.Put(h, p)
		fc.Advance(time.Second)
	}
	// 3000 > 2500: the oldest (0) is evicted.
	if _, ok := c.Get(hs[0]); ok {
		t.Fatal("entry 0 should have been evicted")
	}
	if _, ok := c.Get(hs[1]); !ok {
		t.Fatal("entry 1 missing")
	}
	fc.Advance(time.Second)
	// Touch 1 (done above) so 2 is now least recent; insert 3 → evicts 2.
	p := payload(1000, 3)
	h := sha256.Sum256(p)
	c.Put(h, p)
	if _, ok := c.Get(hs[2]); ok {
		t.Fatal("entry 2 should have been evicted (least recently used)")
	}
	if _, ok := c.Get(hs[1]); !ok {
		t.Fatal("entry 1 (recently used) evicted")
	}
	if c.Used() > 2500 {
		t.Fatalf("Used %d over max", c.Used())
	}
	big := payload(5000, 9)
	c.Put(sha256.Sum256(big), big) // larger than max: not stored, nothing crashes
	if c.Used() > 2500 {
		t.Fatalf("oversize entry stored: Used %d", c.Used())
	}
}

func TestCache_RebuildsOrderOnOpen(t *testing.T) {
	root := t.TempDir()
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c, _ := OpenCache(sysfs.OS{}, root, 10_000, fc)
	var hs [][32]byte
	for i := 0; i < 3; i++ {
		p := payload(1000, int64(i))
		h := sha256.Sum256(p)
		hs = append(hs, h)
		c.Put(h, p)
		fc.Advance(time.Hour)
	}
	// Make entry 0 the most recently used via a Get (refreshes mtime).
	fc.Advance(time.Hour)
	if _, ok := c.Get(hs[0]); !ok {
		t.Fatal("miss")
	}
	c2, err := OpenCache(sysfs.OS{}, root, 2500, fc) // reopen smaller: must evict by mtime order
	if err != nil {
		t.Fatal(err)
	}
	if c2.Used() > 2500 {
		t.Fatalf("Used %d after reopen", c2.Used())
	}
	if _, ok := c2.Get(hs[0]); !ok {
		t.Fatal("most recently used entry evicted on reopen")
	}
	if _, ok := c2.Get(hs[1]); ok {
		t.Fatal("least recently used entry survived reopen")
	}
}

func TestCache_CorruptEntryEvicted(t *testing.T) {
	root := t.TempDir()
	c, _ := OpenCache(sysfs.OS{}, root, 10_000, clock.Real())
	p := payload(1000, 1)
	h := sha256.Sum256(p)
	c.Put(h, p)
	m, _ := filepath.Glob(filepath.Join(root, "cache", "*", "*"))
	if len(m) != 1 {
		t.Fatalf("cache files: %v", m)
	}
	raw, _ := os.ReadFile(m[0])
	raw[5] ^= 1
	_ = os.WriteFile(m[0], raw, 0o644)
	if _, ok := c.Get(h); ok {
		t.Fatal("corrupt cache entry served")
	}
	if _, err := os.Stat(m[0]); err == nil {
		t.Fatal("corrupt cache entry not removed")
	}
	if c.Used() != 0 {
		t.Fatalf("Used = %d", c.Used())
	}
}
