package vfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gurgeh/tailraid/internal/clock"
	"github.com/gurgeh/tailraid/internal/meta"
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
	Size     uint64 // overlay size at capture
	MinSize  uint64 // lowest size since the last successful flush (truncate point)
	SizeSet  bool   // a truncate happened since the last successful flush
	Extents  extents
	Residual extents // subset of Extents the flusher did not commit (partial flush)
}

// dirtyFile is the per-inode overlay state. All fields are guarded by mu.
type dirtyFile struct {
	mu        sync.Mutex
	ino       meta.Ino
	wal       *WAL
	known     bool   // size/base initialised (from a caller or a WAL base record)
	base      uint64 // committed size the WAL records apply to
	size      uint64 // overlay (client-visible) size
	minSize   uint64 // lowest size since the last successful flush
	sizeSet   bool   // truncate since the last successful flush
	ext       extents
	plan      *FlushPlan
	planMin   uint64 // lowest size since the plan was captured
	planTrunc bool   // truncate since the plan was captured
	lastWrite time.Time
}

func (d *dirtyFile) dirty() bool { return len(d.ext) > 0 || d.sizeSet || d.plan != nil }

// Overlay holds the dirty extents and per-inode WALs of one gateway (docs/04 §4–7).
type Overlay struct {
	dir       string
	chunkSize uint32
	clk       clock.Clock
	walCap    int64

	mu    sync.RWMutex
	files map[meta.Ino]*dirtyFile
}

// OpenOverlay opens dir, replaying every `<ino>.wal` into dirty state (restart with a
// dirty buffer, I-37). Torn tails are dropped by the WAL replay.
func OpenOverlay(dir string, chunkSize uint32, clk clock.Clock, opts ...Option) (*Overlay, error) {
	if chunkSize == 0 {
		return nil, errors.New("vfs: chunk size must be > 0")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("vfs: overlay dir: %w", err)
	}
	o := &Overlay{dir: dir, chunkSize: chunkSize, clk: clk, walCap: DefaultWALCap, files: map[meta.Ino]*dirtyFile{}}
	for _, opt := range opts {
		opt(o)
	}
	ents, err := os.ReadDir(dir)
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
	w, err := openWAL(path)
	if err != nil {
		return err
	}
	d := &dirtyFile{ino: ino, wal: w, lastWrite: o.clk.Now()}
	err = w.Replay(func(typ uint8, off uint64, n uint32, walOff int64, _ []byte) error {
		switch typ {
		case recBase:
			d.known, d.base, d.size, d.minSize, d.sizeSet, d.ext = true, off, off, off, false, nil
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
			d.minSize = min(d.minSize, off)
			d.sizeSet = true
		}
		return nil
	})
	if err != nil {
		_ = w.Close()
		return err
	}
	if !d.known {
		// An empty log (crash between Reset and AppendBase) carries no state.
		_ = w.Close()
		return nil
	}
	o.files[ino] = d
	return nil
}

// Close closes every WAL. Dirty state stays on disk for the next OpenOverlay.
func (o *Overlay) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var first error
	for ino, d := range o.files {
		d.mu.Lock()
		if err := d.wal.Close(); err != nil && first == nil {
			first = err
		}
		d.mu.Unlock()
		delete(o.files, ino)
	}
	return first
}

// get returns the locked file entry for ino, creating it (with a base record) on first touch.
func (o *Overlay) get(ino meta.Ino, baseSize uint64, create bool) (*dirtyFile, error) {
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
			w, err := openWAL(filepath.Join(o.dir, strconv.FormatUint(uint64(ino), 10)+".wal"))
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
	if !d.known {
		if err := d.wal.AppendBase(baseSize); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		d.known, d.base, d.size, d.minSize = true, baseSize, baseSize, baseSize
	}
	return d, nil
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
	d.minSize = min(d.minSize, size)
	d.sizeSet = true
	if d.plan != nil {
		d.planMin = min(d.planMin, size)
		d.planTrunc = true
	}
	d.lastWrite = o.clk.Now()
	return nil
}

// Size returns the overlay size of a tracked inode.
func (o *Overlay) Size(ino meta.Ino) (uint64, bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, false
	}
	defer d.mu.Unlock()
	return d.size, true
}

// MinSize returns the lowest size since the last flush (bytes at or beyond it that no
// extent covers read as zero).
func (o *Overlay) MinSize(ino meta.Ino) (uint64, bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, false
	}
	defer d.mu.Unlock()
	return d.minSize, true
}

// Read fills p from the overlay view: base bytes below MinSize, zeros above it, and WAL
// extents (both the in-flight plan's and fresh ones) on top. Returns io.EOF on a short read.
func (o *Overlay) Read(ino meta.Ino, off uint64, p []byte, base func(off uint64, p []byte) error) (int, error) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, meta.ErrNotExist
	}
	defer d.mu.Unlock()
	if off >= d.size {
		return 0, io.EOF
	}
	n := min(uint64(len(p)), d.size-off)
	buf := p[:n]
	// Base bytes are valid only below minSize.
	if off < d.minSize {
		bn := min(n, d.minSize-off)
		if err := base(off, buf[:bn]); err != nil {
			return 0, err
		}
		clear(buf[bn:])
	} else {
		clear(buf)
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
	_ = os.Remove(d.wal.path)
	d.mu.Unlock()
}

// Dirty lists every inode with unflushed state, sorted by Ino.
func (o *Overlay) Dirty() []DirtyInfo {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]DirtyInfo, 0, len(o.files))
	for _, d := range o.files {
		d.mu.Lock()
		if d.dirty() {
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
	defer o.mu.RUnlock()
	var n uint64
	for _, d := range o.files {
		d.mu.Lock()
		n += d.ext.bytes()
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
	d.minSize = min(d.minSize, plan.MinSize)
	d.sizeSet = d.sizeSet || plan.SizeSet
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
	d.base = committedSize
	d.sizeSet = d.planTrunc
	if d.planTrunc {
		d.minSize = min(d.planMin, committedSize)
	} else {
		d.minSize = committedSize
	}
	d.size = max(d.size, d.ext.maxEnd())
	d.plan = nil
	if len(d.ext) == 0 && !d.sizeSet {
		if err := d.wal.Reset(); err != nil {
			return err
		}
		return d.wal.AppendBase(committedSize)
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
	if d.size < d.minSize {
		d.minSize = d.size
	}
	return nil
}
