package vfs

import (
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// ErrWALFull is returned by Write when the inode's WAL has reached its cap; the caller
// must flush the inode before writing more (docs/04 §WAL cap).
var ErrWALFull = errors.New("vfs: wal cap reached, flush required")

// DefaultWALCap is 4 × FileDirtyHi (docs/04).
const DefaultWALCap int64 = 4 * 32 << 20

// Option configures an Overlay.
type Option func(*Overlay)

// WithWALCap sets the per-inode WAL size cap in bytes.
func WithWALCap(n int64) Option { return func(o *Overlay) { o.walCap = n } }

// WithFS routes every WAL file operation through fsys (tests inject faults); the
// default is the real filesystem.
func WithFS(fsys sysfs.FS) Option { return func(o *Overlay) { o.fs = fsys } }

// DirtyInfo describes one inode with unflushed state (docs/04 §flush triggers).
type DirtyInfo struct {
	Ino        meta.Ino
	DirtyBytes uint64 // bytes in extents not yet captured by a flush plan
	Size       uint64
	LastWrite  time.Time
	Flushing   bool // a flush plan is outstanding
}

// FlushPlan is an atomic capture of an inode's dirty state for one flush (docs/04 §7).
// The flusher reads plan extents from the WAL via Overlay.ReadPlan, and may leave some
// extents unflushed by listing them in Residual before CommitFlushed.
type FlushPlan struct {
	Ino      meta.Ino
	Size     uint64 // overlay size at capture; see sizeAgainst for the size a flush uses
	MinSize  uint64 // the truncation point (valid only with SizeSet)
	SizeSet  bool   // a truncate happened since the last successful flush
	Extents  extents
	Residual extents // subset of Extents the flusher did not commit (partial flush)
}

// dirtyFile is the per-inode overlay state. All fields are guarded by mu.
type dirtyFile struct {
	mu        sync.Mutex
	ino       meta.Ino
	wal       *WAL
	known     bool   // initialised (from a caller or a WAL base record)
	size      uint64 // overlay size: extents' end, or the truncate point and later extents; see effSize
	minSize   uint64 // the truncation point (valid only with sizeSet)
	sizeSet   bool   // truncate since the last successful flush
	dead      bool   // evicted from the map; a waiter must look the inode up again
	ext       extents
	plan      *FlushPlan
	planMin   uint64 // lowest size since the plan was captured
	planTrunc bool   // truncate since the plan was captured
	lastWrite time.Time
}

func (d *dirtyFile) dirty() bool { return len(d.ext) > 0 || d.sizeSet || d.plan != nil }

// effSize is the client-visible size given the current committed size: without a
// truncate the overlay has no size of its own, so other gateways' Commits show through
// (L-178); with one, the truncate point and the extents written since decide.
func (d *dirtyFile) effSize(committed uint64) uint64 {
	if d.sizeSet {
		return d.size
	}
	return max(committed, d.extEnd())
}

// extEnd is the end of the last dirty byte, in the plan or fresh.
func (d *dirtyFile) extEnd() uint64 {
	end := d.ext.maxEnd()
	if d.plan != nil {
		end = max(end, d.plan.Extents.maxEnd())
	}
	return end
}

// baseLimit is the offset below which base bytes are valid for a view with the given
// truncate state.
func baseLimit(sizeSet bool, minSize uint64) uint64 {
	if sizeSet {
		return minSize
	}
	return math.MaxUint64
}

// sizeAgainst is the size a flush of this plan commits to when the inode currently
// holds committed bytes: the plan's own size after a truncate, else the larger of the
// committed size and the plan's extents.
func (p *FlushPlan) sizeAgainst(committed uint64) uint64 {
	if p.SizeSet {
		return p.Size
	}
	return max(committed, p.Extents.maxEnd())
}

// Overlay holds the dirty extents and per-inode WALs of one gateway (docs/04 §4–7).
type Overlay struct {
	dir       string
	chunkSize uint32
	clk       clock.Clock
	walCap    int64
	fs        sysfs.FS

	mu    sync.RWMutex
	files map[meta.Ino]*dirtyFile
}

// OpenOverlay opens dir, replaying every `<ino>.wal` into dirty state (restart with a
// dirty buffer, I-37). Torn tails are dropped by the WAL replay.
func OpenOverlay(dir string, chunkSize uint32, clk clock.Clock, opts ...Option) (*Overlay, error) {
	if chunkSize == 0 {
		return nil, errors.New("vfs: chunk size must be > 0")
	}
	o := &Overlay{dir: dir, chunkSize: chunkSize, clk: clk, walCap: DefaultWALCap, fs: sysfs.OS{}, files: map[meta.Ino]*dirtyFile{}}
	for _, opt := range opts {
		opt(o)
	}
	if err := o.fs.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("vfs: overlay dir: %w", err)
	}
	ents, err := o.fs.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("vfs: overlay dir: %w", err)
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".wal") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(name, ".wal"), 10, 64)
		if err != nil {
			continue
		}
		if err := o.replay(meta.Ino(n), filepath.Join(dir, name)); err != nil {
			_ = o.Close()
			return nil, err
		}
	}
	return o, nil
}

func (o *Overlay) replay(ino meta.Ino, path string) error {
	w, err := openWAL(o.fs, path)
	if err != nil {
		return err
	}
	d := &dirtyFile{ino: ino, wal: w, lastWrite: o.clk.Now()}
	err = w.Replay(func(typ uint8, off uint64, n uint32, walOff int64, _ []byte) error {
		switch typ {
		case recBase:
			d.known, d.size, d.minSize, d.sizeSet, d.ext = true, off, 0, false, nil
		case recWrite:
			if !d.known {
				return nil // records before a base are from a corrupt log; ignore
			}
			d.ext.insert(extent{off: off, n: uint64(n), walOff: walOff})
			d.size = max(d.size, off+uint64(n))
		case recTruncate:
			if !d.known {
				return nil
			}
			d.ext.truncate(off)
			d.size = off
			d.setTrunc(off)
		}
		return nil
	})
	if err != nil {
		_ = w.Close()
		return err
	}
	if !d.known || !d.dirty() {
		// An empty log (crash between Reset and AppendBase) or a base record alone
		// (crash between a clean Commit and the removal) carries no state.
		_ = w.Close()
		return o.fs.Remove(path)
	}
	o.files[ino] = d
	return nil
}

// Close closes every WAL. Dirty state stays on disk for the next OpenOverlay.
func (o *Overlay) Close() error {
	o.mu.Lock()
	entries := o.entries()
	o.files = map[meta.Ino]*dirtyFile{}
	o.mu.Unlock()
	var first error
	for _, d := range entries {
		d.mu.Lock()
		if err := d.wal.Close(); err != nil && first == nil {
			first = err
		}
		d.dead = true
		d.mu.Unlock()
	}
	return first
}

// entries snapshots the map (caller holds o.mu). Entry locks are taken only after o.mu
// is released: evict takes them in the opposite order.
func (o *Overlay) entries() []*dirtyFile {
	out := make([]*dirtyFile, 0, len(o.files))
	for _, d := range o.files {
		out = append(out, d)
	}
	return out
}

// get returns the locked file entry for ino, creating it (with a base record) on first touch.
func (o *Overlay) get(ino meta.Ino, baseSize uint64, create bool) (*dirtyFile, error) {
again:
	o.mu.RLock()
	d, ok := o.files[ino]
	o.mu.RUnlock()
	if !ok {
		if !create {
			return nil, nil
		}
		o.mu.Lock()
		d, ok = o.files[ino]
		if !ok {
			w, err := openWAL(o.fs, filepath.Join(o.dir, strconv.FormatUint(uint64(ino), 10)+".wal"))
			if err != nil {
				o.mu.Unlock()
				return nil, err
			}
			d = &dirtyFile{ino: ino, wal: w}
			o.files[ino] = d
		}
		o.mu.Unlock()
	}
	d.mu.Lock()
	if d.dead { // evicted between the lookup and the lock
		d.mu.Unlock()
		goto again
	}
	if !d.known {
		if err := d.wal.AppendBase(baseSize); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		d.known, d.size = true, baseSize
	}
	return d, nil
}

// setTrunc records a truncate to size.
func (d *dirtyFile) setTrunc(size uint64) {
	if !d.sizeSet {
		d.minSize = size
	} else {
		d.minSize = min(d.minSize, size)
	}
	d.sizeSet = true
}

// evict closes and removes the WAL of an entry with nothing to flush. Called with d.mu
// held; the caller must hold no other entry.
func (o *Overlay) evict(d *dirtyFile) error {
	o.mu.Lock()
	if o.files[d.ino] == d {
		delete(o.files, d.ino)
	}
	o.mu.Unlock()
	d.dead = true
	err := d.wal.Close()
	if rerr := o.fs.Remove(d.wal.path); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// Write records p at off. baseSize is the inode's committed size (used on first touch).
func (o *Overlay) Write(ino meta.Ino, baseSize, off uint64, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	d, err := o.get(ino, baseSize, true)
	if err != nil {
		return err
	}
	defer d.mu.Unlock()
	if d.wal.Size()+recHeaderLen+int64(len(p)) > o.walCap {
		return ErrWALFull
	}
	walOff, err := d.wal.AppendWrite(off, p)
	if err != nil {
		return err
	}
	d.ext.insert(extent{off: off, n: uint64(len(p)), walOff: walOff})
	d.size = max(d.size, off+uint64(len(p)))
	d.lastWrite = o.clk.Now()
	return nil
}

// Truncate sets the overlay size and records the truncation point (review #2).
func (o *Overlay) Truncate(ino meta.Ino, baseSize, size uint64) error {
	d, err := o.get(ino, baseSize, true)
	if err != nil {
		return err
	}
	defer d.mu.Unlock()
	if err := d.wal.AppendTruncate(size); err != nil {
		return err
	}
	d.ext.truncate(size)
	d.size = size
	d.setTrunc(size)
	if d.plan != nil {
		d.planMin = min(d.planMin, size)
		d.planTrunc = true
	}
	d.lastWrite = o.clk.Now()
	return nil
}

// Size returns the client-visible size of a dirty inode given its committed size, or
// false when the inode has nothing unflushed.
func (o *Overlay) Size(ino meta.Ino, committed uint64) (uint64, bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, false
	}
	defer d.mu.Unlock()
	return d.effSize(committed), true
}

// MinSize returns the truncation point when a truncate is pending (bytes at or beyond it
// that no extent covers read as zero); without one it returns 0.
func (o *Overlay) MinSize(ino meta.Ino) (uint64, bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, false
	}
	defer d.mu.Unlock()
	return d.minSize, true
}

// Read fills p from the overlay view of an inode whose committed size is committed:
// base bytes below the truncation point (everywhere without one), zeros above it, and
// WAL extents (both the in-flight plan's and fresh ones) on top. base may return fewer
// bytes than asked past the committed size; the rest reads as zero. Returns io.EOF on a
// short read.
func (o *Overlay) Read(ino meta.Ino, committed, off uint64, p []byte, base func(off uint64, p []byte) error) (int, error) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, meta.ErrNotExist
	}
	defer d.mu.Unlock()
	size := d.effSize(committed)
	if off >= size {
		return 0, io.EOF
	}
	n := min(uint64(len(p)), size-off)
	buf := p[:n]
	clear(buf)
	if limit := baseLimit(d.sizeSet, d.minSize); off < limit {
		if err := base(off, buf[:min(n, limit-off)]); err != nil {
			return 0, err
		}
	}
	// Plan extents first, then fresh ones (fresh are newer and win).
	if d.plan != nil {
		if err := d.applyExtents(d.plan.Extents, off, buf); err != nil {
			return 0, err
		}
	}
	if err := d.applyExtents(d.ext, off, buf); err != nil {
		return 0, err
	}
	if uint64(len(p)) > n {
		return int(n), io.EOF
	}
	return int(n), nil
}

func (d *dirtyFile) applyExtents(ext extents, off uint64, buf []byte) error {
	end := off + uint64(len(buf))
	for _, x := range ext.overlapping(off, uint64(len(buf))) {
		s := max(x.off, off)
		e := min(x.end(), end)
		if e <= s {
			continue
		}
		if err := d.wal.ReadAt(buf[s-off:e-off], x.walOff+int64(s-x.off)); err != nil {
			return err
		}
	}
	return nil
}

// Drop forgets an inode (unlink) and removes its WAL. Idempotent.
func (o *Overlay) Drop(ino meta.Ino) {
	o.mu.Lock()
	d, ok := o.files[ino]
	if ok {
		delete(o.files, ino)
	}
	o.mu.Unlock()
	if !ok {
		return
	}
	d.mu.Lock()
	_ = d.wal.Close()
	_ = o.fs.Remove(d.wal.path)
	d.mu.Unlock()
}

// Dirty lists every inode with unflushed state, sorted by Ino.
func (o *Overlay) Dirty() []DirtyInfo {
	o.mu.RLock()
	entries := o.entries()
	o.mu.RUnlock()
	out := make([]DirtyInfo, 0, len(entries))
	for _, d := range entries {
		d.mu.Lock()
		if !d.dead && d.dirty() {
			out = append(out, DirtyInfo{Ino: d.ino, DirtyBytes: d.ext.bytes(), Size: d.size, LastWrite: d.lastWrite, Flushing: d.plan != nil})
		}
		d.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ino < out[j].Ino })
	return out
}

// TotalDirty is the number of bytes in fresh extents across all inodes.
func (o *Overlay) TotalDirty() uint64 {
	o.mu.RLock()
	entries := o.entries()
	o.mu.RUnlock()
	var n uint64
	for _, d := range entries {
		d.mu.Lock()
		if !d.dead {
			n += d.ext.bytes()
		}
		d.mu.Unlock()
	}
	return n
}

// SnapshotForFlush atomically captures the dirty state as a plan and starts a fresh
// extent set. It returns false when nothing is dirty or a plan is already outstanding.
func (o *Overlay) SnapshotForFlush(ino meta.Ino) (*FlushPlan, bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return nil, false
	}
	defer d.mu.Unlock()
	if d.plan != nil || (len(d.ext) == 0 && !d.sizeSet) {
		return nil, false
	}
	p := &FlushPlan{Ino: ino, Size: d.size, MinSize: d.minSize, SizeSet: d.sizeSet, Extents: d.ext.clone()}
	d.plan = p
	d.ext = nil
	d.planMin = d.size
	d.planTrunc = false
	return p, true
}

// ReadPlanRange assembles [off, off+len(p)) for a flush: base bytes below the plan's
// MinSize, zeros at or above it, and only the plan's extents on top (fresh writes belong
// to the next flush).
func (o *Overlay) ReadPlanRange(plan *FlushPlan, off uint64, p []byte, base func(off uint64, p []byte) error) error {
	d, _ := o.get(plan.Ino, 0, false)
	if d == nil {
		return meta.ErrNotExist
	}
	defer d.mu.Unlock()
	clear(p)
	if limit := baseLimit(plan.SizeSet, plan.MinSize); off < limit {
		if err := base(off, p[:min(uint64(len(p)), limit-off)]); err != nil {
			return err
		}
	}
	return d.applyExtents(plan.Extents, off, p)
}

// ReadPlan fills p with the plan extent's bytes starting at x.off+skip.
func (o *Overlay) ReadPlan(plan *FlushPlan, x extent, skip uint64, p []byte) error {
	d, _ := o.get(plan.Ino, 0, false)
	if d == nil {
		return meta.ErrNotExist
	}
	defer d.mu.Unlock()
	return d.wal.ReadAt(p, x.walOff+int64(skip))
}

// AbortFlush returns a plan's extents to the dirty set under any fresh writes (newer
// wins) and clipped by any fresh truncate, so a failed flush loses nothing (I-35).
func (o *Overlay) AbortFlush(plan *FlushPlan) {
	d, _ := o.get(plan.Ino, 0, false)
	if d == nil {
		return
	}
	defer d.mu.Unlock()
	if d.plan != plan {
		return
	}
	d.mergeBack(plan.Extents)
	if plan.SizeSet {
		d.setTrunc(plan.MinSize)
	}
	d.size = max(d.size, d.ext.maxEnd())
	d.plan = nil
}

// mergeBack rebuilds the fresh set as old (clipped to planMin) with the fresh extents on top.
func (d *dirtyFile) mergeBack(old extents) {
	merged := old.clone()
	merged.truncate(d.planMin)
	for _, x := range d.ext {
		merged.insert(x)
	}
	d.ext = merged
}

// CommitFlushed records that the plan was committed with the inode now holding
// committedSize bytes. Residual extents stay dirty; the WAL is reset when nothing remains
// dirty, otherwise compacted to the live records.
func (o *Overlay) CommitFlushed(plan *FlushPlan, committedSize uint64) error {
	d, _ := o.get(plan.Ino, 0, false)
	if d == nil {
		return meta.ErrNotExist
	}
	defer d.mu.Unlock()
	if d.plan != plan {
		return errors.New("vfs: plan is not outstanding")
	}
	d.mergeBack(plan.Residual)
	d.sizeSet = d.planTrunc
	if d.planTrunc {
		d.minSize = min(d.planMin, committedSize)
	} else {
		d.minSize = 0
	}
	d.size = max(d.size, d.ext.maxEnd())
	d.plan = nil
	if len(d.ext) == 0 && !d.sizeSet {
		// nothing left: drop the entry so the next touch starts from the then-committed
		// size instead of pinning this one (L-178)
		return o.evict(d)
	}
	// Compact: base, optional truncate, then the live extents' data.
	recs := []walRec{{typ: recBase, off: committedSize}}
	if d.sizeSet {
		recs = append(recs, walRec{typ: recTruncate, off: d.minSize})
	}
	for _, x := range d.ext {
		data := make([]byte, x.n)
		if err := d.wal.ReadAt(data, x.walOff); err != nil {
			return err
		}
		recs = append(recs, walRec{typ: recWrite, off: x.off, data: data})
	}
	offs, err := d.wal.Compact(recs)
	if err != nil {
		return err
	}
	first := len(recs) - len(d.ext)
	for i := range d.ext {
		d.ext[i].walOff = offs[first+i]
	}
	return nil
}
