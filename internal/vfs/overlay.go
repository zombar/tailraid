package vfs

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// ErrWALFull is returned by Write when the inode's WAL has reached its cap; the caller
// must flush the inode before writing more (docs/04 §WAL cap).
var ErrWALFull = errors.New("vfs: wal cap reached, flush required")

// ErrFileTooLarge is returned by Write and Truncate when the file would exceed
// meta.MaxFileSize for the volume's chunk size (EFBIG to the client, I-64).
var ErrFileTooLarge = errors.New("vfs: file would exceed the maximum size")

// DefaultWALCap is 4 × FileDirtyHi (docs/04).
const DefaultWALCap int64 = 4 * 32 << 20

// DefaultMaxDirty is the gateway-wide dirty-byte cap (docs/04 §3): at or above it a WRITE
// waits for the flusher (DefaultCapWait) and then fails with EIO.
const DefaultMaxDirty uint64 = 1 << 30

// DefaultCapWait is how long a WRITE that meets a cap waits for the flusher to free room
// before it fails: inside every client's soft-mount timeout (docs/04 §3, docs/07, I-79).
const DefaultCapWait = 20 * time.Second

// ErrDirtyFull is returned by Write while the gateway holds MaxDirty or more dirty bytes
// (EIO to the client, L-23).
var ErrDirtyFull = errors.New("vfs: gateway dirty-byte cap reached")

// Option configures an Overlay.
type Option func(*Overlay)

// WithWALCap sets the per-inode WAL size cap in bytes.
func WithWALCap(n int64) Option { return func(o *Overlay) { o.walCap = n } }

// WithMaxDirty sets the gateway-wide dirty-byte cap.
func WithMaxDirty(n uint64) Option { return func(o *Overlay) { o.maxDirty = n } }

// WithCapWait sets how long a WRITE waits at a cap; 0 fails at once.
func WithCapWait(d time.Duration) Option { return func(o *Overlay) { o.capWait = d } }

// WithFS routes every WAL file operation through fsys (tests inject faults); the
// default is the real filesystem.
func WithFS(fsys sysfs.FS) Option { return func(o *Overlay) { o.fs = fsys } }

// WithLog reports what replay discarded after a crash (docs/04 §2); unset is silent.
func WithLog(l *slog.Logger) Option { return func(o *Overlay) { o.Log = l } }

// DirtyInfo describes one inode with unflushed state (docs/04 §flush triggers).
type DirtyInfo struct {
	Ino        meta.Ino
	DirtyBytes uint64 // bytes in extents not yet captured by a flush plan
	PlanBytes  uint64 // bytes in the outstanding flush plan
	WALBytes   int64  // size of the inode's WAL file (every record since the last compaction)
	Size       uint64
	LastWrite  time.Time
	Flushing   bool   // a flush plan is outstanding
	LastErr    string // why the inode's last flush attempt failed; empty when it succeeded
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

	rewriteStart bool   // this flush starts a rewrite (docs/04 §6, I-223)
	rewrite      uint64 // the rewrite this flush continues; 0 for a plain write
	// Actor is the user whose writes the plan carries (docs/21 §4): the Commit acts for
	// them; 0 before users, or for the node's own writer.
	Actor meta.UserIdx
}

// dirtyFile is the per-inode overlay state. All fields are guarded by mu.
type dirtyFile struct {
	mu        sync.Mutex
	ino       meta.Ino
	wal       *WAL
	actor     meta.UserIdx // the last writer (docs/21 §4), from the WAL's actor record
	known     bool         // initialised (from a caller or a WAL base record)
	size      uint64       // overlay size: extents' end, or the truncate point and later extents; see effSize
	minSize   uint64       // the truncation point (valid only with sizeSet)
	sizeSet   bool         // truncate since the last successful flush
	dead      bool         // evicted from the map; a waiter must look the inode up again
	ext       extents
	plan      *FlushPlan
	planMin   uint64 // lowest size since the plan was captured
	planTrunc bool   // truncate since the plan was captured
	counted   uint64 // this entry's share of Overlay.total (fresh + plan bytes)
	lastErr   error  // the last flush attempt's failure, nil after a success (I-79)
	lastWrite time.Time
	// times are the access and modification times a client set while the file was dirty
	// and after its last write, re-applied after each flush's Commit, which stamps its own
	// (I-230); a later write or truncate clears them, being newer
	times *userTimes
	// Group commit (docs/04 §2.1, I-168): a record's in-memory effect waits in pending
	// until its fsync succeeded and is applied in log order; gate holds new appends while
	// a snapshot, compaction or eviction waits for pending to drain.
	cond    *sync.Cond // on mu; broadcast when pending drains or the gate opens
	pending []pendingRec
	gate    bool
}

// pendingRec is an appended record whose effect is applied once its fsync covers end.
type pendingRec struct {
	end   int64
	apply func()
}

func (d *dirtyFile) c() *sync.Cond {
	if d.cond == nil {
		d.cond = sync.NewCond(&d.mu)
	}
	return d.cond
}

// waitGate holds the caller (with d.mu) while a quiesce is in progress; it reports false
// when the entry died meanwhile and the caller must look the inode up again.
func (d *dirtyFile) waitGate() bool {
	for d.gate && !d.dead {
		d.c().Wait()
	}
	return !d.dead
}

// quiesce closes the gate and waits until no append of this inode is between its write
// and its fsync; release reopens it. Called with d.mu held.
func (d *dirtyFile) quiesce() (release func()) {
	d.gate = true
	for len(d.pending) > 0 {
		d.c().Wait()
	}
	return func() {
		d.gate = false
		d.c().Broadcast()
	}
}

// settle applies, in log order, every pending record the WAL has fsynced, and drops
// every record a failed fsync cut back. Called with d.mu held after a syncTo.
func (d *dirtyFile) settle() {
	synced, size := d.wal.Synced(), d.wal.Size()
	kept := d.pending[:0]
	for _, p := range d.pending {
		switch {
		case p.end <= synced:
			p.apply()
		case p.end > size: // cut back: never acknowledged, nothing to apply
		default:
			kept = append(kept, p)
		}
	}
	for i := len(kept); i < len(d.pending); i++ {
		d.pending[i] = pendingRec{}
	}
	d.pending = kept
	d.c().Broadcast()
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
	maxDirty  uint64
	capWait   time.Duration
	fs        sysfs.FS

	// Log, when set, reports what replay discarded after a crash (docs/04 §2); nil is
	// silent (tests).
	Log *slog.Logger

	mu    sync.RWMutex
	files map[meta.Ino]*dirtyFile

	total    atomic.Int64  // dirty bytes, fresh and in outstanding plans, across every inode
	pmu      sync.Mutex    // guards progress
	progress chan struct{} // closed and replaced whenever a flush frees room (cap waiters)

	rmu      sync.Mutex                // guards rewrites
	rewrites map[meta.Ino]rewriteClaim // this gateway's rewrites (docs/04 §6, I-223)
}

// RewriteHold is how long a rewrite claim outlives the last WRITE of the inode through
// this gateway: a copy whose writes pause stays one rewrite, and an in-place edit a
// minute after the copy is a plain write again (docs/04 §6, I-223).
const RewriteHold = time.Minute

// rewriteClaim is a rewrite this gateway started on an inode: pending (id 0) from an
// O_TRUNC open until its start commits, then the version that start produced.
type rewriteClaim struct {
	id uint64
	at time.Time // the open or the last WRITE
}

// touchRewrite is every WRITE's look at ino's claim: a lapsed one is forgotten, so this
// WRITE and the ones after it are plain; a live one is refreshed. A claim whose copy is
// still dirty never lapses: the rest of it lands as the same rewrite, however long its
// flushes retried.
func (o *Overlay) touchRewrite(ino meta.Ino) {
	dirty := o.dirty(ino)
	o.rmu.Lock()
	defer o.rmu.Unlock()
	c, ok := o.rewrites[ino]
	if !ok {
		return
	}
	now := o.clk.Now()
	if now.Sub(c.at) > RewriteHold && !dirty {
		delete(o.rewrites, ino)
		return
	}
	c.at = now
	o.rewrites[ino] = c
}

// MarkRewrite records that a writer opened ino truncated: its next flush starts a
// rewrite, whatever this gateway held for the inode before (docs/04 §6, I-223).
func (o *Overlay) MarkRewrite(ino meta.Ino) {
	o.rmu.Lock()
	defer o.rmu.Unlock()
	if o.rewrites == nil {
		o.rewrites = map[meta.Ino]rewriteClaim{}
	}
	o.rewrites[ino] = rewriteClaim{at: o.clk.Now()}
}

// Rewrite reports ino's claim for the flush about to commit: start when a rewrite is
// pending, else the id the flush's Commits carry (0: a plain write). A flush never
// refreshes a claim and never lapses one: the WRITEs it carries were made under it.
func (o *Overlay) Rewrite(ino meta.Ino) (start bool, id uint64) {
	o.rmu.Lock()
	defer o.rmu.Unlock()
	c, ok := o.rewrites[ino]
	if !ok {
		return false, 0
	}
	return c.id == 0, c.id
}

// SetRewrite records the version ino's rewrite start produced, its id from now on.
func (o *Overlay) SetRewrite(ino meta.Ino, id uint64) {
	o.rmu.Lock()
	defer o.rmu.Unlock()
	if c, ok := o.rewrites[ino]; ok && c.id == 0 {
		c.id = id
		o.rewrites[ino] = c
	}
}

// ForgetPendingRewrite drops ino's claim if its start is still pending: a flush committed
// the copy's first bytes as a plain write (the gate was closed, or the leader refused the
// rewrite), so no later flush may start one, which would truncate what that flush
// committed. A truncate to 0 still in the overlay starts a fresh one (I-223).
func (o *Overlay) ForgetPendingRewrite(ino meta.Ino) {
	o.rmu.Lock()
	defer o.rmu.Unlock()
	if c, ok := o.rewrites[ino]; ok && c.id == 0 {
		delete(o.rewrites, ino)
	}
}

// PruneRewrites forgets every claim that has lapsed and whose copy has landed; the
// flusher's loop calls it so the table holds only the inodes written within RewriteHold
// or still dirty.
func (o *Overlay) PruneRewrites() {
	now := o.clk.Now()
	o.rmu.Lock()
	var lapsed []meta.Ino
	for ino, c := range o.rewrites {
		if now.Sub(c.at) > RewriteHold {
			lapsed = append(lapsed, ino)
		}
	}
	o.rmu.Unlock()
	for _, ino := range lapsed {
		if o.dirty(ino) {
			continue
		}
		o.rmu.Lock()
		if c, ok := o.rewrites[ino]; ok && now.Sub(c.at) > RewriteHold { // a WRITE since refreshed it
			delete(o.rewrites, ino)
		}
		o.rmu.Unlock()
	}
}

// dirty reports whether ino has dirty data or a flush in flight.
func (o *Overlay) dirty(ino meta.Ino) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	_, ok := o.files[ino]
	return ok
}

// OpenOverlay opens dir, replaying every `<ino>.wal` into dirty state (restart with a
// dirty buffer, I-37). Torn tails are dropped by the WAL replay.
func OpenOverlay(dir string, chunkSize uint32, clk clock.Clock, opts ...Option) (*Overlay, error) {
	if chunkSize == 0 {
		return nil, errors.New("vfs: chunk size must be > 0")
	}
	o := &Overlay{
		dir: dir, chunkSize: chunkSize, clk: clk, walCap: DefaultWALCap, maxDirty: DefaultMaxDirty, capWait: DefaultCapWait,
		fs: sysfs.OS{}, files: map[meta.Ino]*dirtyFile{}, progress: make(chan struct{}),
	}
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
		if strings.HasSuffix(name, ".wal.new") { // a crash mid-Compact; the old log is a superset
			if err := o.fs.Remove(filepath.Join(dir, name)); err != nil {
				return nil, fmt.Errorf("vfs: remove stale compaction temp: %w", err)
			}
			continue
		}
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
		case recActor:
			d.actor = meta.UserIdx(off) //nolint:gosec // written by noteActor from a UserIdx
		}
		return nil
	})
	if w.Discarded > 0 && o.Log != nil {
		o.Log.Error("wal replay discarded bytes after the last intact record (torn tail, or a corrupt record and everything after it)",
			"ino", ino, "bytes", w.Discarded, "kept", w.Size())
	}
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
	o.recount(d)
	o.files[ino] = d
	return nil
}

// recount settles d's share of the gateway-wide dirty total. Called with d.mu held after
// anything that changes its fresh extents or its plan (I-80: plan bytes count).
func (o *Overlay) recount(d *dirtyFile) {
	n := d.ext.bytes()
	if d.plan != nil {
		n += d.plan.Extents.bytes()
	}
	if d.dead {
		n = 0
	}
	o.total.Add(int64(n) - int64(d.counted))
	d.counted = n
}

// notify wakes every WRITE waiting at a cap (docs/04 §3).
func (o *Overlay) notify() {
	o.pmu.Lock()
	close(o.progress)
	o.progress = make(chan struct{})
	o.pmu.Unlock()
}

func (o *Overlay) progressCh() <-chan struct{} {
	o.pmu.Lock()
	defer o.pmu.Unlock()
	return o.progress
}

// NoteFlush records how the inode's last flush attempt ended (nil for success). A WRITE
// that meets a cap consults it: it fails at once behind a flush the cluster had no room
// for and waits out any other failure (I-79, L-189, L-219).
func (o *Overlay) NoteFlush(ino meta.Ino, err error) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return
	}
	d.lastErr = err
	d.mu.Unlock()
}

func (o *Overlay) flushErr(ino meta.Ino) error {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return nil
	}
	defer d.mu.Unlock()
	return d.lastErr
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
		// the WAL is opened (and its directory fsynced) outside the global lock: every
		// lookup on every inode would otherwise wait on this fsync; a racing opener's
		// handle to the same file is simply closed again
		w, err := openWAL(o.fs, filepath.Join(o.dir, strconv.FormatUint(uint64(ino), 10)+".wal"))
		if err != nil {
			return nil, err
		}
		o.mu.Lock()
		d, ok = o.files[ino]
		if !ok {
			d = &dirtyFile{ino: ino, wal: w}
			o.files[ino] = d
		}
		o.mu.Unlock()
		if ok {
			_ = w.Close()
		}
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
//
// The file goes before the map entry: a concurrent get then finds the entry, blocks on
// d.mu, sees it dead and starts over on a fresh log. The other order let it adopt the
// doomed file (open, then unlinked under it) and sit at a cap that nothing would clear.
func (o *Overlay) evict(d *dirtyFile) error {
	d.dead = true
	d.c().Broadcast() // a WRITE waiting at the gate looks the inode up again
	o.recount(d)
	err := d.wal.Close()
	if rerr := o.fs.Remove(d.wal.path); rerr != nil && err == nil {
		err = rerr
	}
	o.mu.Lock()
	if o.files[d.ino] == d {
		delete(o.files, d.ino)
	}
	o.mu.Unlock()
	o.notify()
	return err
}

// Write records p at off. baseSize is the inode's committed size (used on first touch).
// At the inode's WAL cap or the gateway's dirty cap it waits for the flusher to free
// room, up to CapWait, and fails only when the wait expires or, at once, when the inode's
// last flush attempt failed because the cluster is full (I-79, L-188, L-189, L-219).
// Write is WriteAs for the node's own writer.
func (o *Overlay) Write(ino meta.Ino, baseSize, off uint64, p []byte) error {
	return o.WriteAs(ino, baseSize, off, p, 0)
}

// WriteAs records a write of actor (docs/21 §4): the flush's Commit acts for the last
// writer.
func (o *Overlay) WriteAs(ino meta.Ino, baseSize, off uint64, p []byte, actor meta.UserIdx) error {
	if len(p) == 0 {
		return nil
	}
	if limit := meta.MaxFileSize(o.chunkSize); off > limit || uint64(len(p)) > limit-off {
		return ErrFileTooLarge
	}
	o.touchRewrite(ino)
	deadline := o.clk.Now().Add(o.capWait)
	for {
		// the channel is taken before the attempt so a flush that frees room between
		// the refused attempt and the wait is not a lost wakeup
		progress := o.progressCh()
		err := o.tryWrite(ino, baseSize, off, p, actor)
		if err == nil || (!errors.Is(err, ErrWALFull) && !errors.Is(err, ErrDirtyFull)) {
			return err
		}
		if ferr := o.flushErr(ino); ferr != nil && errors.Is(ferr, chunk.ErrFull) {
			// the cluster has no room: waiting frees none (L-189). Any other failure (a
			// leader or peer restarting, a codec refusal) passes, and is waited out like
			// a slow flush (L-219)
			return fmt.Errorf("%w: %w", err, ferr)
		}
		remaining := deadline.Sub(o.clk.Now())
		if remaining <= 0 {
			return err
		}
		select {
		case <-progress:
		case <-o.clk.After(remaining):
		}
	}
}

func (o *Overlay) tryWrite(ino meta.Ino, baseSize, off uint64, p []byte, actor meta.UserIdx) error {
	if o.TotalDirty() >= o.maxDirty {
		return ErrDirtyFull // L-23: nothing is accepted until flushes drain the overlay
	}
	d, err := o.get(ino, baseSize, true)
	if err != nil {
		return err
	}
	if !d.waitGate() {
		d.mu.Unlock()
		return o.tryWrite(ino, baseSize, off, p, actor) // evicted while waiting: a fresh entry
	}
	if d.wal.Size() >= o.walCap {
		// The flusher flushes this inode in full at the cap (L-186); until that drains
		// the WAL nothing more is accepted. One record may cross the line, so the cap is
		// a trigger the run loop can observe, not a size the WAL can never reach.
		d.mu.Unlock()
		return ErrWALFull
	}
	if err := d.noteActor(actor); err != nil {
		d.mu.Unlock()
		return err
	}
	// the record goes to the log under the lock; its fsync is waited for without it, so
	// the WRITEs that arrive meanwhile share that fsync (group commit, I-168); the extent
	// is applied only once the fsync covered the record (settle), in log order
	walOff, end, err := d.wal.write(recWrite, off, p)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	n := uint64(len(p))
	d.pending = append(d.pending, pendingRec{end: end, apply: func() {
		d.ext.insert(extent{off: off, n: n, walOff: walOff})
		d.size = max(d.size, off+n)
		d.lastWrite = o.clk.Now()
		d.times = nil
		o.recount(d)
	}})
	d.mu.Unlock()
	err = d.wal.syncTo(end)
	d.mu.Lock()
	d.settle()
	d.mu.Unlock()
	return err
}

// Truncate sets the overlay size and records the truncation point (review #2).
func (o *Overlay) Truncate(ino meta.Ino, baseSize, size uint64) error {
	return o.TruncateAs(ino, baseSize, size, 0)
}

// TruncateAs is Truncate for actor (docs/21 §4).
func (o *Overlay) TruncateAs(ino meta.Ino, baseSize, size uint64, actor meta.UserIdx) error {
	if size > meta.MaxFileSize(o.chunkSize) {
		return ErrFileTooLarge
	}
	d, err := o.get(ino, baseSize, true)
	if err != nil {
		return err
	}
	if !d.waitGate() {
		d.mu.Unlock()
		return o.TruncateAs(ino, baseSize, size, actor)
	}
	if err := d.noteActor(actor); err != nil {
		d.mu.Unlock()
		return err
	}
	// a truncate is a record like a write: its effect is applied in log order once its
	// fsync covers it, so it never reorders against a write in flight (I-168)
	_, end, err := d.wal.write(recTruncate, size, nil)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	d.pending = append(d.pending, pendingRec{end: end, apply: func() {
		d.ext.truncate(size)
		d.size = size
		d.setTrunc(size)
		if d.plan != nil {
			d.planMin = min(d.planMin, size)
			d.planTrunc = true
		}
		d.lastWrite = o.clk.Now()
		d.times = nil
		o.recount(d)
	}})
	d.mu.Unlock()
	err = d.wal.syncTo(end)
	d.mu.Lock()
	d.settle()
	d.mu.Unlock()
	return err
}

// noteActor records a change of writer in the log (docs/21 §4, I-353): the record is
// written under the lock before the write it precedes and the Commit acts for the last
// writer. Called with d.mu held.
func (d *dirtyFile) noteActor(actor meta.UserIdx) error {
	if actor == d.actor {
		return nil
	}
	if _, _, err := d.wal.write(recActor, uint64(actor), nil); err != nil {
		return err
	}
	d.actor = actor
	return nil
}

// Actor is the last writer of a dirty inode (0 for none, or the node itself).
func (o *Overlay) Actor(ino meta.Ino) meta.UserIdx {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0
	}
	defer d.mu.Unlock()
	return d.actor
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

// userTimes are a client's access and modification times, in nanoseconds (meta.SetAttr).
type userTimes struct{ atime, mtime int64 }

// NoteTimes records the times a client set on ino, when it has dirty data: the flusher
// sets them again after it commits that data (I-230). A file with nothing dirty needs
// nothing: no Commit follows to stamp over them.
func (o *Overlay) NoteTimes(ino meta.Ino, atime, mtime int64) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return
	}
	defer d.mu.Unlock()
	if d.dirty() {
		d.times = &userTimes{atime: atime, mtime: mtime}
	}
}

// Changed reports a dirty file's times as this gateway sees them (docs/04 §3, I-261):
// its last write or truncate, as mtime and ctime, or for mtime the time a client set
// after it (I-230). ok is false when ino has nothing dirty.
func (o *Overlay) Changed(ino meta.Ino) (wrote time.Time, user int64, hasUser, ok bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return time.Time{}, 0, false, false
	}
	defer d.mu.Unlock()
	if !d.dirty() {
		return time.Time{}, 0, false, false
	}
	if d.times != nil {
		return d.lastWrite, d.times.mtime, true, true
	}
	return d.lastWrite, 0, false, true
}

// UserTimes returns the times NoteTimes recorded on ino and no write has superseded.
func (o *Overlay) UserTimes(ino meta.Ino) (atime, mtime int64, ok bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, 0, false
	}
	defer d.mu.Unlock()
	if d.times == nil {
		return 0, 0, false
	}
	return d.times.atime, d.times.mtime, true
}

// DataEnd reports what the overlay holds of an inode's data: the end of its last dirty
// byte (a plan's below the lowest truncate since it was captured, fresh ones whole) and
// the offset below which committed bytes still show. ok is false when the inode has no
// dirty state, and then the committed inode is all there is.
func (o *Overlay) DataEnd(ino meta.Ino) (dirtyEnd, baseLim uint64, ok bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, 0, false
	}
	defer d.mu.Unlock()
	end := d.ext.maxEnd()
	if d.plan != nil {
		end = max(end, min(d.plan.Extents.maxEnd(), d.planMin))
	}
	return end, baseLimit(d.sizeSet, d.minSize), true
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
	size := d.effSize(committed)
	limit := baseLimit(d.sizeSet, d.minSize)
	end := min(off+uint64(len(p)), size)
	// the plan's extents say nothing about bytes at or past planMin: a commit from
	// another gateway may have grown the file there, and those bytes live in the base
	covered := d.ext.covers(off, end) || (d.plan != nil && end <= d.planMin && d.plan.Extents.covers(off, end))
	d.mu.Unlock()
	if off >= size {
		return 0, io.EOF
	}
	n := min(uint64(len(p)), size-off)
	buf := p[:n]
	clear(buf)
	// The base is fetched with the inode unlocked: a chunk fetch can take a network round
	// trip and must not stall WRITEs to the same inode. Dirty bytes are applied on top
	// afterwards from the state of that moment, so a write that lands meanwhile wins. A
	// range the dirty bytes cover entirely needs no base at all.
	if off < limit && !covered {
		if err := base(off, buf[:min(n, limit-off)]); err != nil {
			return 0, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dead { // evicted meanwhile: the base bytes are the committed version, whole (L-73)
		return int(n), nil
	}
	if now := baseLimit(d.sizeSet, d.minSize); now < limit { // truncated meanwhile: base bytes above the new point are gone
		from := max(now, off)
		if from < off+n {
			clear(buf[from-off:])
		}
	}
	// Plan extents first, then fresh ones (fresh are newer and win). Plan bytes at or
	// beyond the lowest truncation point since the snapshot are dead (L-181): the commit
	// still carries them, but minSize hides them afterwards and reads must not show them.
	if d.plan != nil {
		if err := d.applyExtents(d.plan.Extents, off, buf, d.planMin); err != nil {
			return 0, err
		}
	}
	if err := d.applyExtents(d.ext, off, buf, math.MaxUint64); err != nil {
		return 0, err
	}
	if uint64(len(p)) > n {
		return int(n), io.EOF
	}
	return int(n), nil
}

// applyExtents copies the bytes of ext within [off, off+len(buf)) and below limit into buf.
func (d *dirtyFile) applyExtents(ext extents, off uint64, buf []byte, limit uint64) error {
	end := min(off+uint64(len(buf)), limit)
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
	o.mu.RLock()
	d, ok := o.files[ino]
	o.mu.RUnlock()
	if !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dead {
		return
	}
	defer d.quiesce()() // the log goes away: no append may be mid-fsync (I-168)
	if d.dead {
		return
	}
	_ = o.evict(d)
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
			info := DirtyInfo{Ino: d.ino, DirtyBytes: d.ext.bytes(), WALBytes: d.wal.Size(), Size: d.size, LastWrite: d.lastWrite, Flushing: d.plan != nil}
			if d.plan != nil {
				info.PlanBytes = d.plan.Extents.bytes()
			}
			if d.lastErr != nil {
				info.LastErr = d.lastErr.Error()
			}
			out = append(out, info)
		}
		d.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ino < out[j].Ino })
	return out
}

// TotalDirty is the number of dirty bytes across all inodes, fresh and in outstanding
// plans (I-80). It is a counter, so a WRITE checking the cap locks no other inode.
func (o *Overlay) TotalDirty() uint64 { return uint64(max(o.total.Load(), 0)) }

// WALCap is the per-inode WAL size at which the flusher flushes the inode in full.
func (o *Overlay) WALCap() int64 { return o.walCap }

// SnapshotForFlush atomically captures the dirty state as a plan and starts a fresh
// extent set. It returns false when nothing is dirty or a plan is already outstanding.
func (o *Overlay) SnapshotForFlush(ino meta.Ino) (*FlushPlan, bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return nil, false
	}
	defer d.mu.Unlock()
	defer d.quiesce()() // an append between its write and its fsync is not in the snapshot (I-168)
	if d.plan != nil || (len(d.ext) == 0 && !d.sizeSet) {
		return nil, false
	}
	p := &FlushPlan{Ino: ino, Size: d.size, MinSize: d.minSize, SizeSet: d.sizeSet, Extents: d.ext.clone(), Actor: d.actor}
	d.plan = p
	d.ext = nil
	d.planMin = d.size
	d.planTrunc = false
	o.recount(d)
	return p, true
}

// ReadPlanRange assembles [off, off+len(p)) for a flush: base bytes below the plan's
// MinSize, zeros at or above it, and only the plan's extents on top (fresh writes belong
// to the next flush).
//
// The base is fetched before the inode is locked (it may be a network round trip and
// must not stall WRITEs), and not at all when the plan's extents cover the whole range
// (an aligned full-chunk write assembles without a read-modify-write fetch, L-7).
func (o *Overlay) ReadPlanRange(plan *FlushPlan, off uint64, p []byte, base func(off uint64, p []byte) error) error {
	clear(p)
	if limit := baseLimit(plan.SizeSet, plan.MinSize); off < limit && !plan.Extents.covers(off, off+uint64(len(p))) {
		if err := base(off, p[:min(uint64(len(p)), limit-off)]); err != nil {
			return err
		}
	}
	d, _ := o.get(plan.Ino, 0, false)
	if d == nil {
		return meta.ErrNotExist
	}
	defer d.mu.Unlock()
	return d.applyExtents(plan.Extents, off, p, math.MaxUint64)
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
	o.recount(d)
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
	defer d.quiesce()() // the compaction rewrites the log: no append may be mid-fsync (I-168)
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
	o.recount(d)
	defer o.notify() // room for the WRITEs waiting at a cap (L-188)
	if len(d.ext) == 0 && !d.sizeSet {
		// nothing left: drop the entry so the next touch starts from the then-committed
		// size instead of pinning this one (L-178)
		return o.evict(d)
	}
	// Compact: base, the truncation point, the raised size if a truncate-up happened
	// while the plan was outstanding (L-180), then the live extents' data (last, so the
	// record offsets below line up).
	recs := []walRec{{typ: recBase, off: committedSize}}
	if d.actor != 0 {
		recs = append(recs, walRec{typ: recActor, off: uint64(d.actor)})
	}
	if d.sizeSet {
		recs = append(recs, walRec{typ: recTruncate, off: d.minSize})
		if d.size > d.minSize {
			recs = append(recs, walRec{typ: recTruncate, off: d.size})
		}
	}
	for _, x := range d.ext {
		recs = append(recs, walRec{typ: recWrite, off: x.off, src: x.walOff, n: x.n})
	}
	offs, err := d.wal.Compact(recs)
	if offs != nil { // the new log is in place even when a step after the rename failed
		first := len(recs) - len(d.ext)
		for i := range d.ext {
			d.ext[i].walOff = offs[first+i]
		}
	}
	return err
}
