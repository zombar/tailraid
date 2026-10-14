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
	"strings"
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
	// pinned entries stay in the LRU but eviction passes over them (docs/01 §Pins): the
	// budget max bounds the unpinned bytes, so pinned bytes may exceed it
	pinned     map[[32]byte]struct{}
	pinnedUsed int64

	memMax  int64
	mem     *list.List // front = most recently used
	memIdx  map[[32]byte]*list.Element
	memUsed int64

	// disk writes run behind the Put that accounted the entry (I-204): at most
	// CacheWriters at once, at most CacheQueue outstanding; past that a Put writes inline
	writeSlots chan struct{}
	writing    int // Puts whose write is outstanding (under mu)
	wg         sync.WaitGroup
}

// CacheWriters is how many disk writes the cache runs at once; CacheQueue is how many
// Puts may have their write outstanding before a Put writes synchronously, which bounds
// the chunk buffers the pending writes hold to CacheQueue chunks (docs/04 §9, I-204).
const (
	CacheWriters = 2
	CacheQueue   = 8
)

type cacheEntry struct {
	hash    [32]byte
	size    int64
	touched time.Time     // last mtime refresh (LRU order across restarts)
	pending chan struct{} // closed once the file is on disk; nil when it is
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
	c := &Cache{
		fsys: fsys, root: root, max: maxBytes, clk: clk, lru: list.New(), idx: map[[32]byte]*list.Element{},
		pinned: map[[32]byte]struct{}{},
		memMax: DefaultMemory, mem: list.New(), memIdx: map[[32]byte]*list.Element{},
		writeSlots: make(chan struct{}, CacheWriters),
	}
	for _, o := range opts {
		o(c)
	}
	if err := privateTree(fsys, c.dir()); err != nil {
		return nil, err
	}
	type ent struct {
		cacheEntry
		mtime time.Time
	}
	var ents []ent
	err := walkTwoLevel(fsys, c.dir(), func(dir string, de fs.DirEntry) error {
		if strings.HasPrefix(de.Name(), tmpPrefix) {
			// a Put cut short by a crash; one process owns the cache, so nothing is
			// mid-write at open (the same rule as the shard store's temps, L-127)
			return fsys.Remove(filepath.Join(dir, de.Name()))
		}
		b, err := hex.DecodeString(de.Name())
		if err != nil || len(b) != 32 {
			return nil //nolint:nilerr // a name that is not a hash is a foreign file: skipped, not an error
		}
		fi, err := de.Info()
		if err != nil {
			return nil //nolint:nilerr // vanished between readdir and stat: nothing to account
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
	c.removeEvicted(c.evictLocked())
	return c, nil
}

func (c *Cache) dir() string { return filepath.Join(c.root, "cache") }
func (c *Cache) path(h [32]byte) string {
	hx := hex.EncodeToString(h[:])
	return filepath.Join(c.dir(), hx[:2], hx)
}

// Used returns the bytes currently cached, pinned ones included.
func (c *Cache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Max is the budget for the unpinned bytes (cache.size).
func (c *Cache) Max() int64 { return c.max }

// Pinned returns the bytes held by pinned entries.
func (c *Cache) Pinned() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinnedUsed
}

// Writing reports the Puts whose disk write is still outstanding; never above CacheQueue.
func (c *Cache) Writing() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writing
}

// Drain waits for every outstanding disk write.
func (c *Cache) Drain() { c.wg.Wait() }

// Close drains the writes; the cache is purely local, so nothing else needs closing.
func (c *Cache) Close() { c.Drain() }

// Has reports whether h is cached, without touching its LRU position (readahead and the
// pins worker ask before deciding to fetch).
func (c *Cache) Has(h [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.idx[h]
	return ok
}

// Pin keeps h out of eviction while it stays pinned and reports whether the cache holds
// it: false means the entry is not cached (never was, or was evicted meanwhile) and the
// caller fetches it again. Pinning a pinned entry is a no-op.
func (c *Cache) Pin(h [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.idx[h]
	if !ok {
		return false
	}
	if _, p := c.pinned[h]; !p {
		c.pinned[h] = struct{}{}
		c.pinnedUsed += el.Value.(cacheEntry).size
	}
	return true
}

// Unpin returns h to the LRU: it is evicted like any other entry once the unpinned bytes
// exceed the budget. Unpinning an unknown or unpinned hash is a no-op.
func (c *Cache) Unpin(h [32]byte) {
	c.mu.Lock()
	if _, p := c.pinned[h]; !p {
		c.mu.Unlock()
		return
	}
	delete(c.pinned, h)
	if el, ok := c.idx[h]; ok {
		c.pinnedUsed -= el.Value.(cacheEntry).size
	}
	victims := c.evictLocked()
	c.mu.Unlock()
	c.removeEvicted(victims)
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
	pending := el.Value.(cacheEntry).pending
	c.mu.Unlock()
	if pending != nil { // the bytes are on their way to disk: wait rather than miss and refetch (I-204)
		<-pending
		c.mu.Lock()
		_, still := c.idx[h]
		c.mu.Unlock()
		if !still { // the write failed and dropped the entry
			return nil, false
		}
	}

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

// Put stores data (whose sha256 must be h) and evicts least-recently-used entries until
// the cache fits in max. Entries larger than max are ignored. The entry is accounted and
// in memory when Put returns; its file is written behind it unless CacheQueue writes are
// already outstanding, in which case this Put writes it before returning (I-204).
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
	done := make(chan struct{})
	c.idx[h] = c.lru.PushFront(cacheEntry{hash: h, size: int64(len(data)), touched: c.clk.Now(), pending: done})
	c.used += int64(len(data))
	victims := c.evictLocked()
	c.memPutLocked(h, data)
	async := c.writing < CacheQueue
	if async {
		c.writing++
		c.wg.Add(1)
	}
	c.mu.Unlock()
	c.removeEvicted(victims)
	if !async {
		c.write(h, data, done)
		return
	}
	go func() {
		defer c.wg.Done()
		c.writeSlots <- struct{}{}
		c.write(h, data, done)
		<-c.writeSlots
		c.mu.Lock()
		c.writing--
		c.mu.Unlock()
	}()
}

// write puts data's file in place and settles the entry that done belongs to: the
// entry loses its pending mark, or, when it was evicted meanwhile, the file goes again;
// a failed write drops the entry so a Get misses instead of finding no file.
func (c *Cache) write(h [32]byte, data []byte, done chan struct{}) {
	defer close(done)
	p := c.path(h)
	written := c.writeFile(p, data)
	c.mu.Lock()
	el, ok := c.idx[h]
	if ok && el.Value.(cacheEntry).pending == done {
		if written {
			e := el.Value.(cacheEntry)
			e.pending = nil
			el.Value = e
			c.mu.Unlock()
			_ = c.fsys.Chtimes(p, e.touched, e.touched) // the LRU time is the Put's (or a hit's refresh since), not the write's
			return
		}
		c.used -= el.Value.(cacheEntry).size
		if _, pinned := c.pinned[h]; pinned {
			delete(c.pinned, h)
			c.pinnedUsed -= el.Value.(cacheEntry).size
		}
		c.lru.Remove(el)
		delete(c.idx, h)
		c.memDropLocked(h)
	}
	c.mu.Unlock()
	if written { // evicted or dropped while the file was being written: it is untracked
		_ = c.fsys.Remove(p)
	}
}

// writeFile writes data to p through a temp beside it and reports whether p now exists.
func (c *Cache) writeFile(p string, data []byte) bool {
	if err := c.fsys.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false
	}
	f, err := c.fsys.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return false
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = c.fsys.Remove(f.Name())
		return false
	}
	if err := f.Close(); err != nil {
		_ = c.fsys.Remove(f.Name())
		return false
	}
	if err := c.fsys.Rename(f.Name(), p); err != nil {
		_ = c.fsys.Remove(f.Name())
		return false
	}
	return true
}

// evictLocked untracks least-recently-used unpinned entries until the unpinned bytes fit
// in max and returns them; the caller removes their files after unlocking
// (removeEvicted), so a slow disk never holds the lock. Pinned entries are passed over.
func (c *Cache) evictLocked() []cacheEntry {
	var victims []cacheEntry
	for el := c.lru.Back(); el != nil && c.used-c.pinnedUsed > c.max; {
		prev := el.Prev()
		e := el.Value.(cacheEntry)
		if _, p := c.pinned[e.hash]; !p {
			c.lru.Remove(el)
			delete(c.idx, e.hash)
			c.memDropLocked(e.hash)
			c.used -= e.size
			victims = append(victims, e)
		}
		el = prev
	}
	return victims
}

// removeEvicted deletes evicted files. One whose remove fails is tracked again (at the
// least-recent end) so a later eviction retries it instead of leaking it until restart.
func (c *Cache) removeEvicted(victims []cacheEntry) {
	for _, e := range victims {
		err := c.fsys.Remove(c.path(e.hash))
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		c.mu.Lock()
		if _, ok := c.idx[e.hash]; !ok {
			c.idx[e.hash] = c.lru.PushBack(e)
			c.used += e.size
		}
		c.mu.Unlock()
	}
}

func (c *Cache) drop(h [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[h]; ok {
		size := el.Value.(cacheEntry).size
		c.used -= size
		if _, p := c.pinned[h]; p { // corrupt on disk: the pin worker fetches it again
			delete(c.pinned, h)
			c.pinnedUsed -= size
		}
		c.lru.Remove(el)
		delete(c.idx, h)
	}
	c.memDropLocked(h)
	_ = c.fsys.Remove(c.path(h))
}
