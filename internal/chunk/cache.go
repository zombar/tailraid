package chunk

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
)

// Cache is a bounded LRU of decoded chunks under root/cache/<hh>/<hex>, keyed by chunk
// hash. It is purely local, verified on read, and may be deleted at any time. Writes are
// not fsynced: losing the cache costs a re-fetch, never data. The hottest entries are
// also held in memory (a bounded subset of the disk entries, verified when inserted), so
// a repeated READ of one chunk costs neither a disk read nor a hash.
type Cache struct {
	fsys sysfs.FS
	root string
	max  int64
	clk  clock.Clock

	mu   sync.Mutex
	lru  *list.List // front = most recently used
	idx  map[[32]byte]*list.Element
	used int64

	memMax  int64
	mem     *list.List // front = most recently used
	memIdx  map[[32]byte]*list.Element
	memUsed int64
}

type cacheEntry struct {
	hash    [32]byte
	size    int64
	touched time.Time // last mtime refresh (LRU order across restarts)
}

type memEntry struct {
	hash [32]byte
	data []byte
}

// DefaultMemory is the in-memory tier's size unless WithMemory says otherwise.
const DefaultMemory = 64 << 20

// touchEvery bounds how often a hit refreshes the entry's mtime: the on-disk order only
// matters across restarts, so one refresh a minute keeps it close enough.
const touchEvery = time.Minute

// CacheOption tunes a Cache.
type CacheOption func(*Cache)

// WithMemory sets the in-memory tier's size in bytes (0 disables it).
func WithMemory(n int64) CacheOption { return func(c *Cache) { c.memMax = n } }

// OpenCache scans root/cache and rebuilds the LRU order from file mtimes, evicting down
// to max if the directory holds more than that.
func OpenCache(fsys sysfs.FS, root string, maxBytes int64, clk clock.Clock, opts ...CacheOption) (*Cache, error) {
	c := &Cache{fsys: fsys, root: root, max: maxBytes, clk: clk, lru: list.New(), idx: map[[32]byte]*list.Element{},
		memMax: DefaultMemory, mem: list.New(), memIdx: map[[32]byte]*list.Element{}}
	for _, o := range opts {
		o(c)
	}
	if err := fsys.MkdirAll(c.dir(), 0o755); err != nil {
		return nil, err
	}
	type ent struct {
		cacheEntry
		mtime time.Time
	}
	var ents []ent
	err := walkTwoLevel(fsys, c.dir(), func(_ string, de fs.DirEntry) error {
		b, err := hex.DecodeString(de.Name())
		if err != nil || len(b) != 32 {
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			return nil
		}
		var h [32]byte
		copy(h[:], b)
		ents = append(ents, ent{cacheEntry{hash: h, size: fi.Size(), touched: fi.ModTime()}, fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].mtime.After(ents[j].mtime) })
	for _, e := range ents { // most recent first → PushBack keeps front = most recent
		c.idx[e.hash] = c.lru.PushBack(e.cacheEntry)
		c.used += e.size
	}
	c.evictLocked()
	return c, nil
}

func (c *Cache) dir() string { return filepath.Join(c.root, "cache") }
func (c *Cache) path(h [32]byte) string {
	hx := hex.EncodeToString(h[:])
	return filepath.Join(c.dir(), hx[:2], hx)
}

// Used returns the bytes currently cached.
func (c *Cache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Get returns the chunk for h if cached and intact: from memory when hot, else from
// disk (verified, then promoted to memory). A corrupt disk entry is removed. The slice
// is shared with the cache and must not be modified.
func (c *Cache) Get(h [32]byte) ([]byte, bool) {
	c.mu.Lock()
	el, ok := c.idx[h]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	c.lru.MoveToFront(el)
	refresh := c.dueForTouchLocked(el)
	if mel, hot := c.memIdx[h]; hot {
		c.mem.MoveToFront(mel)
		data := mel.Value.(memEntry).data
		c.mu.Unlock()
		if refresh {
			now := c.clk.Now()
			_ = c.fsys.Chtimes(c.path(h), now, now)
		}
		return data, true
	}
	c.mu.Unlock()

	f, err := c.fsys.Open(c.path(h))
	if err != nil {
		c.drop(h)
		return nil, false
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || sha256.Sum256(data) != h {
		c.drop(h)
		return nil, false
	}
	if refresh {
		now := c.clk.Now()
		_ = c.fsys.Chtimes(c.path(h), now, now) // keeps LRU order across restarts
	}
	c.mu.Lock()
	c.memPutLocked(h, data)
	c.mu.Unlock()
	return data, true
}

// dueForTouchLocked reports whether the entry's mtime should be refreshed now and
// records the refresh.
func (c *Cache) dueForTouchLocked(el *list.Element) bool {
	e := el.Value.(cacheEntry)
	now := c.clk.Now()
	if now.Sub(e.touched) < touchEvery {
		return false
	}
	e.touched = now
	el.Value = e
	return true
}

// memPutLocked adds data to the memory tier (only for an entry the disk tier holds) and
// evicts its least recently used entries down to memMax.
func (c *Cache) memPutLocked(h [32]byte, data []byte) {
	if int64(len(data)) > c.memMax {
		return
	}
	if _, ok := c.idx[h]; !ok {
		return
	}
	if mel, ok := c.memIdx[h]; ok {
		c.mem.MoveToFront(mel)
		return
	}
	c.memIdx[h] = c.mem.PushFront(memEntry{h, data})
	c.memUsed += int64(len(data))
	for c.memUsed > c.memMax && c.mem.Len() > 0 {
		c.memDropLocked(c.mem.Back().Value.(memEntry).hash)
	}
}

func (c *Cache) memDropLocked(h [32]byte) {
	if mel, ok := c.memIdx[h]; ok {
		c.memUsed -= int64(len(mel.Value.(memEntry).data))
		c.mem.Remove(mel)
		delete(c.memIdx, h)
	}
}

// MemoryUsed is the size of the in-memory tier.
func (c *Cache) MemoryUsed() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memUsed
}

// Put stores data (whose sha256 must be h) and evicts least-recently-used entries until
// the cache fits in max. Entries larger than max are ignored.
func (c *Cache) Put(h [32]byte, data []byte) {
	if int64(len(data)) > c.max {
		return
	}
	c.mu.Lock()
	if el, ok := c.idx[h]; ok {
		c.lru.MoveToFront(el)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	p := c.path(h)
	if err := c.fsys.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := c.fsys.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = c.fsys.Remove(f.Name())
		return
	}
	if err := f.Close(); err != nil {
		_ = c.fsys.Remove(f.Name())
		return
	}
	if err := c.fsys.Rename(f.Name(), p); err != nil {
		_ = c.fsys.Remove(f.Name())
		return
	}
	now := c.clk.Now()
	_ = c.fsys.Chtimes(p, now, now)

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.idx[h]; ok { // raced with another Put of the same hash
		return
	}
	c.idx[h] = c.lru.PushFront(cacheEntry{hash: h, size: int64(len(data)), touched: now})
	c.used += int64(len(data))
	c.evictLocked()
	c.memPutLocked(h, data)
}

func (c *Cache) evictLocked() {
	for c.used > c.max && c.lru.Len() > 0 {
		el := c.lru.Back()
		e := el.Value.(cacheEntry)
		c.lru.Remove(el)
		delete(c.idx, e.hash)
		c.memDropLocked(e.hash)
		c.used -= e.size
		if err := c.fsys.Remove(c.path(e.hash)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
	}
}

func (c *Cache) drop(h [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[h]; ok {
		c.used -= el.Value.(cacheEntry).size
		c.lru.Remove(el)
		delete(c.idx, h)
	}
	c.memDropLocked(h)
	_ = c.fsys.Remove(c.path(h))
}
