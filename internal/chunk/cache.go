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

	"github.com/gurgeh/tailraid/internal/clock"
	"github.com/gurgeh/tailraid/internal/sysfs"
)

// Cache is a bounded LRU of decoded chunks under root/cache/<hh>/<hex>, keyed by chunk
// hash. It is purely local, verified on read, and may be deleted at any time. Writes are
// not fsynced: losing the cache costs a re-fetch, never data.
type Cache struct {
	fsys sysfs.FS
	root string
	max  int64
	clk  clock.Clock

	mu   sync.Mutex
	lru  *list.List // front = most recently used
	idx  map[[32]byte]*list.Element
	used int64
}

type cacheEntry struct {
	hash [32]byte
	size int64
}

// OpenCache scans root/cache and rebuilds the LRU order from file mtimes, evicting down
// to max if the directory holds more than that.
func OpenCache(fsys sysfs.FS, root string, maxBytes int64, clk clock.Clock) (*Cache, error) {
	c := &Cache{fsys: fsys, root: root, max: maxBytes, clk: clk, lru: list.New(), idx: map[[32]byte]*list.Element{}}
	if err := fsys.MkdirAll(c.dir(), 0o755); err != nil {
		return nil, err
	}
	type ent struct {
		cacheEntry
		mtime time.Time
	}
	var ents []ent
	subs, err := fsys.ReadDir(c.dir())
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if !sub.IsDir() {
			continue
		}
		files, err := fsys.ReadDir(filepath.Join(c.dir(), sub.Name()))
		if err != nil {
			return nil, err
		}
		for _, de := range files {
			b, err := hex.DecodeString(de.Name())
			if err != nil || len(b) != 32 || de.IsDir() {
				continue
			}
			fi, err := de.Info()
			if err != nil {
				continue
			}
			var h [32]byte
			copy(h[:], b)
			ents = append(ents, ent{cacheEntry{h, fi.Size()}, fi.ModTime()})
		}
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

// Get returns the chunk for h if cached and intact. A corrupt entry is removed.
func (c *Cache) Get(h [32]byte) ([]byte, bool) {
	c.mu.Lock()
	el, ok := c.idx[h]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	c.lru.MoveToFront(el)
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
	now := c.clk.Now()
	_ = c.fsys.Chtimes(c.path(h), now, now) // keeps LRU order across restarts
	return data, true
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
	c.idx[h] = c.lru.PushFront(cacheEntry{h, int64(len(data))})
	c.used += int64(len(data))
	c.evictLocked()
}

func (c *Cache) evictLocked() {
	for c.used > c.max && c.lru.Len() > 0 {
		el := c.lru.Back()
		e := el.Value.(cacheEntry)
		c.lru.Remove(el)
		delete(c.idx, e.hash)
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
	_ = c.fsys.Remove(c.path(h))
}
