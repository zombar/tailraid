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
	defer c.Drain()
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
	defer c.Drain()
	var hs [][32]byte
	for i := range 3 {
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
	defer c.Drain()
	var hs [][32]byte
	for i := range 3 {
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
	c.Drain()
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

// Lifecycle: L-67
func TestCache_CorruptEntryEvicted(t *testing.T) {
	root := t.TempDir()
	c, _ := OpenCache(sysfs.OS{}, root, 10_000, clock.Real(), WithMemory(0)) // every hit reads the disk
	p := payload(1000, 1)
	h := sha256.Sum256(p)
	c.Put(h, p)
	c.Drain()
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

// The memory tier serves a hot chunk without touching the disk, is bounded, and never
// outlives the disk entry it mirrors (a disk eviction or drop empties it too).
func TestCache_MemoryTier(t *testing.T) {
	root := t.TempDir()
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c, err := OpenCache(sysfs.OS{}, root, 10_000, fc, WithMemory(2000))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Drain()
	var hs [][32]byte
	for i := range 3 {
		p := payload(1000, int64(i))
		hs = append(hs, sha256.Sum256(p))
		c.Put(hs[i], p)
	}
	c.Drain()
	if c.MemoryUsed() != 2000 { // 3000 written, 2000 fit: entry 0 left memory, stays on disk
		t.Fatalf("memory %d", c.MemoryUsed())
	}
	files, _ := filepath.Glob(filepath.Join(root, "cache", "*", "*"))
	if len(files) != 3 {
		t.Fatalf("disk files %v", files)
	}
	for _, f := range files { // the disk is gone: memory hits still serve, misses fail
		_ = os.Remove(f)
	}
	if got, ok := c.Get(hs[2]); !ok || !bytes.Equal(got, payload(1000, 2)) {
		t.Fatal("hot entry not served from memory")
	}
	if _, ok := c.Get(hs[0]); ok {
		t.Fatal("cold entry served without its file")
	}
	// a disk eviction drops the memory copy as well
	c2, _ := OpenCache(sysfs.OS{}, t.TempDir(), 2500, fc, WithMemory(10_000))
	defer c2.Drain()
	for i := range 3 {
		p := payload(1000, int64(i))
		c2.Put(sha256.Sum256(p), p)
		fc.Advance(time.Second)
	}
	if c2.MemoryUsed() != 2000 {
		t.Fatalf("memory after disk eviction %d", c2.MemoryUsed())
	}
	if _, ok := c2.Get(sha256.Sum256(payload(1000, 0))); ok {
		t.Fatal("disk-evicted entry served from memory")
	}
}

// A hit refreshes the entry's mtime at most once a minute (the order only matters across
// restarts), not on every READ.
func TestCache_TouchThrottled(t *testing.T) {
	root := t.TempDir()
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c, _ := OpenCache(sysfs.OS{}, root, 10_000, fc)
	defer c.Drain()
	p := payload(1000, 1)
	h := sha256.Sum256(p)
	c.Put(h, p)
	c.Drain()
	mtime := func() time.Time {
		files, _ := filepath.Glob(filepath.Join(root, "cache", "*", "*"))
		fi, err := os.Stat(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}
	t0 := mtime()
	fc.Advance(30 * time.Second)
	c.Get(h)
	if !mtime().Equal(t0) {
		t.Fatal("mtime refreshed within a minute of the last touch")
	}
	fc.Advance(31 * time.Second)
	c.Get(h)
	if mtime().Equal(t0) {
		t.Fatal("mtime not refreshed after a minute")
	}
}
