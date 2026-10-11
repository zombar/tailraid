// Package pins keeps folders on a device (docs/01 §Local chunk cache, docs/12 §10,
// I-162): a pinned folder's chunks are fetched into the node's chunk cache and held out
// of its eviction while the pin stands. Pins are local state in <data_dir>/pins.json,
// stored by inode so a rename keeps a pin and a delete makes it lapse; nothing about them
// is replicated.
package pins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

const (
	// File is the pin list's name under the data directory.
	File = "pins.json"
	// Interval is how often the worker looks for a moved applied index.
	Interval = 30 * time.Second
	// Fetchers bounds the chunk reads a walk has in flight.
	Fetchers = 2
)

// ErrRoot is returned for a pin of the volume's root: a pin is a folder.
var ErrRoot = errors.New("pins: the root cannot be pinned; pin a folder")

// Pin is one pinned folder as the file records it: the inode is the identity, the path
// the label at pin time.
type Pin struct {
	Ino  meta.Ino `json:"ino"`
	Gen  uint32   `json:"gen"`
	Path string   `json:"path"`
}

// Entry is one pin as status reports it.
type Entry struct {
	Path        string `json:"path"`
	Bytes       uint64 `json:"bytes"`        // file bytes under the folder, each chunk once
	CachedBytes uint64 `json:"cached_bytes"` // of those, the bytes the cache holds now
}

// Report is the cache block of `status --json` and `/api/pins`.
type Report struct {
	Size        int64   `json:"size"`         // cache.size, the budget for unpinned bytes
	Used        int64   `json:"used"`         // bytes cached, pinned ones included
	PinnedBytes int64   `json:"pinned_bytes"` // bytes held by pins
	Pins        []Entry `json:"pins"`
}

// Store is the metadata the worker walks: *meta.FSM.
type Store interface {
	GetInode(meta.Ino) (meta.Inode, bool)
	ReadDir(meta.Ino) ([]meta.Inode, error)
	Resolve([]string) (meta.Inode, error)
	PathOf(meta.Ino) ([]string, error)
	GetChunk(meta.Hash) (meta.Chunk, bool)
	Applied() uint64
}

// Reader fetches a chunk into the cache: *vfs.Reader.
type Reader interface {
	ReadChunk(ctx context.Context, h meta.Hash) ([]byte, error)
}

// Pins is a node's pin list and the worker that keeps the cache filled for it.
type Pins struct {
	file  string
	st    Store
	rd    Reader
	cache *chunk.Cache
	clk   clock.Clock
	log   *slog.Logger

	mu     sync.Mutex
	pins   []Pin
	held   map[meta.Hash]struct{} // what the last walk pinned in the cache
	cancel context.CancelFunc     // the running walk's, so an unpin stops it
	kick   chan struct{}

	walkMu sync.Mutex // one walk at a time
}

// Open loads <dataDir>/pins.json (absent: no pins).
func Open(dataDir string, st Store, rd Reader, cache *chunk.Cache, clk clock.Clock, log *slog.Logger) (*Pins, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Pins{file: filepath.Join(dataDir, File), st: st, rd: rd, cache: cache, clk: clk, log: log, held: map[meta.Hash]struct{}{}, kick: make(chan struct{}, 1)}
	b, err := os.ReadFile(p.file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, nil
	case err != nil:
		return nil, fmt.Errorf("pins: %w", err)
	}
	if err := json.Unmarshal(b, &p.pins); err != nil {
		return nil, fmt.Errorf("pins: %s: %w", p.file, err)
	}
	return p, nil
}

// saveLocked writes the list atomically.
func (p *Pins) saveLocked() error {
	if p.pins == nil {
		p.pins = []Pin{} // an empty list, not null
	}
	b, err := json.MarshalIndent(p.pins, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("pins: %w", err)
	}
	if err := os.Rename(tmp, p.file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pins: %w", err)
	}
	return nil
}

// Pin keeps the folder at path on this device: it is recorded, the worker is woken to
// fetch it, and its entry (with the bytes it covers) is returned. Pinning a pinned
// folder is a no-op that returns the same entry.
func (p *Pins) Pin(path string) (Entry, error) {
	parts := meta.SplitPath(path)
	if len(parts) == 0 {
		return Entry{}, ErrRoot
	}
	in, err := p.st.Resolve(parts)
	if err != nil {
		return Entry{}, err
	}
	if in.Type != meta.TDir {
		return Entry{}, meta.ErrNotDir
	}
	pin := Pin{Ino: in.Ino, Gen: in.Gen, Path: "/" + strings.Join(parts, "/")}
	p.mu.Lock()
	if !slices.ContainsFunc(p.pins, func(x Pin) bool { return x.Ino == in.Ino }) {
		p.pins = append(p.pins, pin)
		err = p.saveLocked()
	}
	p.mu.Unlock()
	if err != nil {
		return Entry{}, err
	}
	p.wake()
	e, _ := p.entry(pin)
	return e, nil
}

// Unpin releases the folder at path (or, when the path no longer resolves, the pin
// recorded under that label): its chunks return to the LRU at once, and a walk in
// progress is cancelled.
func (p *Pins) Unpin(path string) error {
	parts := meta.SplitPath(path)
	label := "/" + strings.Join(parts, "/")
	var ino meta.Ino
	if in, err := p.st.Resolve(parts); err == nil {
		ino = in.Ino
	}
	p.mu.Lock()
	i := slices.IndexFunc(p.pins, func(x Pin) bool { return (ino != 0 && x.Ino == ino) || x.Path == label })
	if i < 0 {
		p.mu.Unlock()
		return fmt.Errorf("pins: %s is not pinned: %w", label, meta.ErrNotExist)
	}
	p.pins = slices.Delete(p.pins, i, i+1)
	err := p.saveLocked()
	if p.cancel != nil {
		p.cancel()
	}
	want := p.wantLocked()
	p.releaseLocked(want)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	p.wake()
	return nil
}

// List reports the cache and every pin that still resolves.
func (p *Pins) List() Report {
	p.mu.Lock()
	pins := slices.Clone(p.pins)
	p.mu.Unlock()
	rep := Report{Size: p.cache.Max(), Used: p.cache.Used(), PinnedBytes: p.cache.Pinned(), Pins: []Entry{}}
	for _, pin := range pins {
		if e, ok := p.entry(pin); ok {
			rep.Pins = append(rep.Pins, e)
		}
	}
	return rep
}

// entry describes one pin; ok is false when its folder is gone (deleted or trashed).
func (p *Pins) entry(pin Pin) (Entry, bool) {
	in, ok := p.st.GetInode(pin.Ino)
	if !ok || in.Gen != pin.Gen || in.Type != meta.TDir {
		return Entry{}, false
	}
	e := Entry{Path: pin.Path}
	if parts, err := p.st.PathOf(pin.Ino); err == nil {
		e.Path = "/" + strings.Join(parts, "/")
	}
	want := map[meta.Hash]int64{}
	p.collect(pin.Ino, want)
	for h, size := range want {
		e.Bytes += uint64(size)
		if p.cache.Has(h) {
			e.CachedBytes += uint64(size)
		}
	}
	return e, true
}

// collect adds every chunk under dir to want, each hash once with its size.
func (p *Pins) collect(dir meta.Ino, want map[meta.Hash]int64) {
	ents, err := p.st.ReadDir(dir)
	if err != nil {
		return
	}
	for i := range ents {
		switch ents[i].Type {
		case meta.TDir:
			p.collect(ents[i].Ino, want)
		case meta.TFile:
			for _, h := range ents[i].Chunks {
				if h == meta.HashZero {
					continue
				}
				if _, seen := want[h]; seen {
					continue
				}
				if ch, ok := p.st.GetChunk(h); ok {
					want[h] = int64(ch.Size)
				}
			}
		case meta.TSymlink:
		}
	}
}

// wantLocked is every chunk under every pin that still resolves; the lapsed pins are
// returned so the caller drops them.
func (p *Pins) wantLocked() map[meta.Hash]int64 {
	want := map[meta.Hash]int64{}
	for _, pin := range p.pins {
		if in, ok := p.st.GetInode(pin.Ino); ok && in.Gen == pin.Gen && in.Type == meta.TDir {
			p.collect(pin.Ino, want)
		}
	}
	return want
}

// releaseLocked unpins what the last walk held and no pin wants any more.
func (p *Pins) releaseLocked(want map[meta.Hash]int64) {
	for h := range p.held {
		if _, ok := want[h]; !ok {
			p.cache.Unpin(h)
			delete(p.held, h)
		}
	}
}

func (p *Pins) wake() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Walk fetches every chunk under every pin the cache lacks (Fetchers at a time), pins
// them all, releases what no pin wants any more, and drops the pins whose folder is
// gone. A chunk that cannot be read now is left for the next walk; the error says how
// many.
func (p *Pins) Walk(ctx context.Context) error {
	p.walkMu.Lock()
	defer p.walkMu.Unlock()
	// a prefetch is background work: its shard requests hold at most half of a node's
	// slots and queue behind client reads (docs/06 §4, I-175)
	ctx, cancel := context.WithCancel(chunk.Background(ctx))
	defer cancel()
	p.mu.Lock()
	p.cancel = cancel
	var keep []Pin
	for _, pin := range p.pins {
		if in, ok := p.st.GetInode(pin.Ino); ok && in.Gen == pin.Gen && in.Type == meta.TDir {
			keep = append(keep, pin)
		} else {
			p.log.Info("pin lapsed: its folder is gone", "path", pin.Path)
		}
	}
	var saveErr error
	if len(keep) != len(p.pins) {
		p.pins = keep
		saveErr = p.saveLocked()
	}
	want := p.wantLocked()
	p.mu.Unlock()
	if saveErr != nil {
		return saveErr
	}

	var todo []meta.Hash
	for h := range want {
		if !p.cache.Pin(h) {
			todo = append(todo, h)
		}
	}
	slices.SortFunc(todo, func(a, b meta.Hash) int { return strings.Compare(string(a[:]), string(b[:])) })
	sem := make(chan struct{}, Fetchers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	missed := 0
	for _, h := range todo {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(h meta.Hash) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := p.rd.ReadChunk(ctx, h)
			if err == nil && !p.cache.Pin(h) {
				err = errors.New("evicted before it was pinned")
			}
			if err != nil {
				mu.Lock()
				missed++
				mu.Unlock()
				p.log.Debug("pin fetch", "chunk", fmt.Sprintf("%x", h[:8]), "err", err)
			}
		}(h)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.mu.Lock()
	p.releaseLocked(want)
	for h := range want {
		if p.cache.Has(h) {
			p.held[h] = struct{}{}
		}
	}
	p.cancel = nil
	p.mu.Unlock()
	if missed > 0 {
		return fmt.Errorf("pins: %d chunks not fetched this walk", missed)
	}
	return nil
}

// Run is the worker: a walk at start, on every pin, and every Interval when the FSM's
// applied index has moved since the last walk; it returns when ctx ends.
func (p *Pins) Run(ctx context.Context) {
	last := uint64(0)
	walk := func() {
		last = p.st.Applied()
		if err := p.Walk(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("pin walk", "err", err)
		}
	}
	walk()
	for {
		t := p.clk.NewTimer(Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-p.kick:
			t.Stop()
			walk()
		case <-t.C():
			if p.st.Applied() != last {
				walk()
			}
		}
	}
}
