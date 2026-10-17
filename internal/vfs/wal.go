package vfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/zombar/tailraid/internal/sysfs"
)

// WAL record types (docs/04 §2.1).
const (
	recWrite    uint8 = 1 // off = file offset, data = bytes
	recTruncate uint8 = 2 // off = new size, no data
	recBase     uint8 = 3 // off = committed size the following records apply to, no data
	recActor    uint8 = 4 // off = the user whose writes follow (docs/21 §4, I-353), no data; a reader from before ignores it
	recState    uint8 = 5 // off = overlay size, data = minSize u64 | sizeSet u8: a segment's first record (I-397)
	recClean    uint8 = 6 // off = W, data = size | minSize | sizeSet | ranges…: what a Commit made durable (I-397)
)

// Record layout: crc32c(u32 LE, over everything after it) | type u8 | off u64 LE | len u32 LE | data.
const recHeaderLen = 4 + 1 + 8 + 4

// maxRecordData bounds a single record so a corrupt length can never allocate unboundedly.
const maxRecordData = 64 << 20

// SegmentSize is the size at which an inode's head segment is rotated (docs/04 §2.1).
const SegmentSize = 64 << 20

// segShift places segment n's bytes at log positions n·2⁴⁰ + offset, so positions grow
// across segments and an extent's walOff names its segment.
const segShift = 40

func segPos(n int, off int64) int64 { return int64(n)<<segShift | off }
func segOf(pos int64) int           { return int(pos >> segShift) }
func segOff(pos int64) int64        { return pos & (1<<segShift - 1) }

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var errWALShortRead = errors.New("vfs: wal read beyond end")

// ErrWALClosed is every call on a WAL whose file could not be reopened after a failed
// fsync or compaction (I-101): the inode's writes fail with it until the gateway restarts
// and replays the log, rather than touching a handle that is closed or stale.
var ErrWALClosed = errors.New("vfs: wal unusable until restart")

// walRec is a record to be written by Compact: a write record names its bytes by their
// position and length in the log being compacted, so nothing is read ahead of the copy.
type walRec struct {
	typ uint8
	off uint64
	src int64  // data position in the current log (recWrite)
	n   uint64 // data length (recWrite)
}

// compactBuf bounds the copy of one record during a compaction (docs/04 §2, I-80).
const compactBuf = 1 << 20

// segment is one file of an inode's log.
type segment struct {
	n    int
	size int64
}

// WAL is a per-inode append-only log, a run of segment files of which the newest, the
// head, takes appends (docs/04 §2.1). Every append is fsynced before it returns, so a
// successful append means the bytes survive a crash or power loss (I-7). Concurrent
// appends share fsyncs (group commit, docs/04 §2.1, I-168): a record is written under mu,
// then its caller waits for an fsync that started after the write; the first waiter runs
// it and it covers every record written before it began. All file access goes through
// the sysfs.FS seam so tests can inject fsync failures and crashes.
type WAL struct {
	fs   sysfs.FS
	path string // segment 0's file, <ino>.wal; segment n is <ino>.<n>.wal beside it

	mu      sync.Mutex // guards f, segs, size, synced, syncing, failed and the reader
	cond    *sync.Cond // broadcast when an fsync ends
	f       sysfs.File // the head
	segs    []segment  // oldest first; the last is the head
	size    int64      // log position after the last byte written (synced or not)
	synced  int64      // log position a successful fsync covers
	syncing bool       // an fsync is running
	failed  error      // ErrWALClosed with its cause, once the file could not be reopened
	rn      int        // the older segment rf reads, -1 for none
	rf      sysfs.File
	// Discarded is the number of bytes Replay cut off after the last intact record: a
	// torn tail from a crash, or everything after a record that failed its CRC.
	Discarded int64
}

// errWALCut is returned to an append whose record was cut back by another append's
// failed fsync: it was never acknowledged and is not on disk.
var errWALCut = errors.New("vfs: wal fsync failed; record cut back")

// segPath is segment n's file name next to segment 0's.
func (w *WAL) segPath(n int) string {
	if n == 0 {
		return w.path
	}
	return strings.TrimSuffix(w.path, ".wal") + "." + strconv.Itoa(n) + ".wal"
}

// segmentsOf lists the segment numbers present for the log at path (segment 0's name),
// ascending.
func segmentsOf(fsys sysfs.FS, path string) ([]int, error) {
	dir, base := filepath.Split(path)
	stem := strings.TrimSuffix(base, ".wal") + "."
	ents, err := fsys.ReadDir(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range ents {
		name := e.Name()
		if name == base {
			out = append(out, 0)
			continue
		}
		if rest, ok := strings.CutPrefix(name, stem); ok {
			if num, ok := strings.CutSuffix(rest, ".wal"); ok {
				if n, err := strconv.Atoi(num); err == nil && n > 0 {
					out = append(out, n)
				}
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// openWAL opens the log whose segment 0 is path: every segment present, the newest as
// the head; with none, a new segment 0. It lists the directory, so it is for replay.
func openWAL(fsys sysfs.FS, path string) (*WAL, error) {
	ns, err := segmentsOf(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("vfs: open wal: %w", err)
	}
	return openSegments(fsys, path, ns)
}

// newWAL opens the log of an inode the overlay holds no log for: segment 0 alone, created
// if absent. A WRITE's first touch of an inode lists no directory.
func newWAL(fsys sysfs.FS, path string) (*WAL, error) {
	var ns []int
	if _, err := fsys.Stat(path); err == nil {
		ns = []int{0}
	}
	return openSegments(fsys, path, ns)
}

// openSegments opens the log of path's segments ns (ascending), the newest as the head.
func openSegments(fsys sysfs.FS, path string, ns []int) (*WAL, error) {
	w := &WAL{fs: fsys, path: path, rn: -1}
	w.cond = sync.NewCond(&w.mu)
	for _, n := range ns {
		st, err := fsys.Stat(w.segPath(n))
		if err != nil {
			return nil, fmt.Errorf("vfs: stat wal: %w", err)
		}
		w.segs = append(w.segs, segment{n: n, size: st.Size()})
	}
	head := 0
	if len(w.segs) > 0 {
		head = w.segs[len(w.segs)-1].n
	}
	f, err := w.openSeg(head)
	if err != nil {
		return nil, err
	}
	w.f = f
	if len(w.segs) == 0 {
		w.segs = []segment{{n: 0}}
	}
	w.size = segPos(head, w.segs[len(w.segs)-1].size)
	w.synced = w.size
	return w, nil
}

// openSeg opens (creating) segment n for appends; a new file's directory entry is made
// durable before any record it holds is acknowledged (I-63).
func (w *WAL) openSeg(n int) (sysfs.File, error) {
	f, err := w.fs.OpenFile(w.segPath(n), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vfs: open wal: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vfs: stat wal: %w", err)
	}
	if st.Size() == 0 {
		if err := w.fs.SyncDir(filepath.Dir(w.path)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("vfs: fsync wal dir: %w", err)
		}
	}
	return f, nil
}

// Size is the log position after the last byte written, synced or not; in segment 0 it
// is the file's length.
func (w *WAL) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// Synced is the log position that a successful fsync covers.
func (w *WAL) Synced() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.synced
}

// Bytes is the length of every segment together: what the log holds on disk.
func (w *WAL) Bytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytesLocked()
}

func (w *WAL) bytesLocked() int64 {
	var n int64
	for _, s := range w.segs[:len(w.segs)-1] {
		n += s.size
	}
	return n + segOff(w.size)
}

// HeadBytes is the length of the head segment.
func (w *WAL) HeadBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return segOff(w.size)
}

// Close closes the head and any open reader.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeReaderLocked()
	return w.f.Close()
}

func (w *WAL) closeReaderLocked() {
	if w.rf != nil {
		_ = w.rf.Close()
		w.rf, w.rn = nil, -1
	}
}

// RemoveAll closes the log and removes every segment.
func (w *WAL) RemoveAll() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeReaderLocked()
	err := w.f.Close()
	for _, s := range w.segs {
		if rerr := w.fs.Remove(w.segPath(s.n)); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
			err = rerr
		}
	}
	return err
}

func encodeRecord(typ uint8, off uint64, data []byte) []byte {
	buf := make([]byte, recHeaderLen+len(data))
	buf[4] = typ
	binary.LittleEndian.PutUint64(buf[5:], off)
	binary.LittleEndian.PutUint32(buf[13:], uint32(len(data)))
	copy(buf[recHeaderLen:], data)
	binary.LittleEndian.PutUint32(buf[0:], crc32.Checksum(buf[4:], crcTable))
	return buf
}

// write puts one record at the end of the log, unsynced; it returns the position of the
// record's data and the end of the log after it, which syncTo takes (I-168).
func (w *WAL) write(typ uint8, off uint64, data []byte) (dataOff, end int64, err error) {
	if len(data) > maxRecordData {
		return 0, 0, fmt.Errorf("vfs: wal record too large (%d bytes)", len(data))
	}
	buf := encodeRecord(typ, off, data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return 0, 0, w.failed
	}
	if _, err := w.f.WriteAt(buf, segOff(w.size)); err != nil {
		return 0, 0, fmt.Errorf("vfs: wal write: %w", err)
	}
	dataOff = w.size + recHeaderLen
	w.size += int64(len(buf))
	return dataOff, w.size, nil
}

// syncTo returns once an fsync that began after the log reached end has succeeded. The
// first caller to find no fsync running runs one covering everything written so far;
// the others wait for it and, if it did not cover them, for the next (group commit).
// A failed fsync cuts the log back to the last synced byte: every record past it is gone
// and its append fails (I-37), and the file is reopened before any later append (I-101).
func (w *WAL) syncTo(end int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		switch {
		case w.synced >= end:
			return nil
		case w.failed != nil:
			return w.failed
		case end > w.size:
			return errWALCut // cut back by an earlier group's failed fsync
		case w.syncing:
			w.cond.Wait()
			continue
		}
		w.syncing = true
		target := w.size
		f := w.f
		w.mu.Unlock()
		err := f.Sync()
		w.mu.Lock()
		w.syncing = false
		if err == nil {
			w.synced = target
		} else {
			w.cutBackLocked()
		}
		w.cond.Broadcast()
		if err != nil {
			return fmt.Errorf("vfs: wal fsync: %w", err)
		}
	}
}

// cutBackLocked drops every unsynced record after a failed fsync so a crash cannot
// replay it (I-37), then reopens the file: after a failed fsync the kernel may have
// dropped the dirty pages and cleared the error on this descriptor, so the next fsync
// on it would report success for data that never reached the disk (I-101).
func (w *WAL) cutBackLocked() {
	head := segOf(w.synced)
	_ = w.f.Truncate(segOff(w.synced))
	_ = w.f.Sync()
	_ = w.f.Close()
	w.size = w.synced
	if f, rerr := w.fs.OpenFile(w.segPath(head), os.O_RDWR, 0o600); rerr == nil {
		w.f = f
	} else {
		w.failed = fmt.Errorf("%w: reopen after a failed fsync: %w", ErrWALClosed, rerr)
	}
}

// Rotate starts a new head segment holding first (the state records of docs/04 §2.1).
// The caller has quiesced the inode's appends (I-168), so every record of the old head
// is fsynced and one fsync never spans two files; first is fsynced with the next append.
func (w *WAL) Rotate(first ...[]byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return w.failed
	}
	if w.syncing || w.synced != w.size {
		return errors.New("vfs: wal rotate with an append in flight (bug)")
	}
	next := segOf(w.size) + 1
	f, err := w.openSeg(next)
	if err != nil {
		return err
	}
	var pos int64
	for _, rec := range first {
		if _, err := f.WriteAt(rec, pos); err != nil {
			_ = f.Close()
			_ = w.fs.Remove(w.segPath(next))
			return fmt.Errorf("vfs: wal rotate: %w", err)
		}
		pos += int64(len(rec))
	}
	w.segs[len(w.segs)-1].size = segOff(w.size)
	_ = w.f.Close()
	w.f = f
	w.segs = append(w.segs, segment{n: next})
	w.synced = segPos(next, 0)
	w.size = segPos(next, pos)
	return nil
}

// DropBefore removes, oldest first, every segment that lies wholly below log position
// pos; the head always stays. A crash part-way leaves a suffix of the run (I-397).
func (w *WAL) DropBefore(pos int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	keep := min(segOf(pos), segOf(w.size))
	for len(w.segs) > 1 && w.segs[0].n < keep {
		if w.rn == w.segs[0].n {
			w.closeReaderLocked() // Windows refuses to remove an open file
		}
		if err := w.fs.Remove(w.segPath(w.segs[0].n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("vfs: wal drop segment: %w", err)
		}
		w.segs = w.segs[1:]
	}
	return nil
}

// AppendWrite logs a write of p at file offset off. The returned walOff addresses p's bytes.
func (w *WAL) AppendWrite(off uint64, p []byte) (int64, error) {
	return w.append(recWrite, off, p)
}

// AppendTruncate logs a truncate to size.
func (w *WAL) AppendTruncate(size uint64) error {
	_, err := w.append(recTruncate, size, nil)
	return err
}

// AppendBase logs the committed size that subsequent records apply to (first record).
func (w *WAL) AppendBase(size uint64) error {
	_, err := w.append(recBase, size, nil)
	return err
}

// append writes one record at the end and fsyncs; returns the position of its data.
func (w *WAL) append(typ uint8, off uint64, data []byte) (int64, error) {
	dataOff, end, err := w.write(typ, off, data)
	if err != nil {
		return 0, err
	}
	return dataOff, w.syncTo(end)
}

// fileFor returns the file holding segment n for a read (caller holds mu): the head, or
// an older segment opened on demand and kept open until the next one is read.
func (w *WAL) fileFor(n int) (sysfs.File, error) {
	if n == segOf(w.size) {
		return w.f, nil
	}
	if w.rn == n {
		return w.rf, nil
	}
	w.closeReaderLocked()
	f, err := w.fs.Open(w.segPath(n))
	if err != nil {
		return nil, fmt.Errorf("vfs: wal read: %w", err)
	}
	w.rf, w.rn = f, n
	return f, nil
}

// segSize is the length of segment n (caller holds mu), -1 when it is not in the log.
func (w *WAL) segSize(n int) int64 {
	if n == segOf(w.size) {
		return segOff(w.size)
	}
	for _, s := range w.segs {
		if s.n == n {
			return s.size
		}
	}
	return -1
}

// ReadAt fills p with logged bytes starting at walOff (a data position returned by
// AppendWrite, possibly advanced into the record by an extent split).
func (w *WAL) ReadAt(p []byte, walOff int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return w.failed
	}
	n, off := segOf(walOff), segOff(walOff)
	if size := w.segSize(n); walOff < 0 || size < 0 || off+int64(len(p)) > size {
		return errWALShortRead
	}
	f, err := w.fileFor(n)
	if err != nil {
		return err
	}
	if _, err := f.ReadAt(p, off); err != nil {
		return fmt.Errorf("vfs: wal read: %w", err)
	}
	return nil
}

// Replay calls fn for every intact record in order, segment by segment. It stops silently
// at the first record whose length or CRC does not check out (a torn tail from a crash
// mid-append), truncates that segment to its last intact record and removes any later
// segment, which nothing acknowledged can depend on (I-37).
func (w *WAL) Replay(fn func(typ uint8, off uint64, n uint32, walOff int64, data []byte) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	hdr := make([]byte, recHeaderLen)
	for i := range w.segs {
		s := w.segs[i]
		size := w.segSize(s.n)
		// an older segment gets a handle of its own: fn may read the log meanwhile,
		// which moves the shared reader
		f, own := w.f, sysfs.File(nil)
		if s.n != segOf(w.size) {
			var err error
			if own, err = w.fs.Open(w.segPath(s.n)); err != nil {
				return fmt.Errorf("vfs: wal replay: %w", err)
			}
			f = own
		}
		pos, err := w.replaySeg(f, s.n, size, hdr, fn)
		if own != nil {
			_ = own.Close()
		}
		if err != nil {
			return err
		}
		if pos < size {
			return w.cutTailLocked(i, pos)
		}
	}
	return nil
}

// replaySeg replays segment n (size bytes, read through f) up to its first torn record
// and returns where it stopped. Called with mu held; fn runs without it.
func (w *WAL) replaySeg(f sysfs.File, n int, size int64, hdr []byte, fn func(typ uint8, off uint64, n uint32, walOff int64, data []byte) error) (int64, error) {
	var pos int64
	for pos+recHeaderLen <= size {
		if _, err := f.ReadAt(hdr, pos); err != nil {
			return pos, fmt.Errorf("vfs: wal replay: %w", err)
		}
		ln := binary.LittleEndian.Uint32(hdr[13:])
		if ln > maxRecordData || pos+recHeaderLen+int64(ln) > size {
			break
		}
		data := make([]byte, ln)
		if ln > 0 {
			if _, err := f.ReadAt(data, pos+recHeaderLen); err != nil {
				return pos, fmt.Errorf("vfs: wal replay: %w", err)
			}
		}
		crc := crc32.Update(crc32.Checksum(hdr[4:], crcTable), crcTable, data)
		if crc != binary.LittleEndian.Uint32(hdr[0:]) {
			break
		}
		typ := hdr[4]
		off := binary.LittleEndian.Uint64(hdr[5:])
		w.mu.Unlock()
		err := fn(typ, off, ln, segPos(n, pos+recHeaderLen), data)
		w.mu.Lock()
		if err != nil {
			return pos, err
		}
		pos += recHeaderLen + int64(ln)
	}
	return pos, nil
}

// cutTailLocked ends the log at position pos of segment index i: the segment is
// truncated there and every later one removed; the cut segment becomes the head.
func (w *WAL) cutTailLocked(i int, pos int64) error {
	s := w.segs[i]
	discarded := w.segSize(s.n) - pos
	for _, later := range w.segs[i+1:] {
		discarded += w.segSize(later.n)
	}
	w.Discarded = discarded
	w.closeReaderLocked()
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("vfs: wal truncate torn tail: %w", err)
	}
	for _, later := range w.segs[i+1:] {
		if err := w.fs.Remove(w.segPath(later.n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("vfs: wal remove after torn tail: %w", err)
		}
	}
	w.segs = w.segs[:i+1]
	f, err := w.fs.OpenFile(w.segPath(s.n), os.O_RDWR, 0o600)
	if err != nil {
		w.failed = fmt.Errorf("%w: reopen after a torn tail: %w", ErrWALClosed, err)
		return w.failed
	}
	w.f = f
	if err := f.Truncate(pos); err != nil {
		return fmt.Errorf("vfs: wal truncate torn tail: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("vfs: wal fsync: %w", err)
	}
	w.size, w.synced = segPos(s.n, pos), segPos(s.n, pos)
	return nil
}

// Compact atomically replaces the log with exactly recs, written to a fresh segment after
// the head, returning each record's data position in it; the older segments are then
// removed, oldest first. A write record's bytes are streamed from the current log
// through one bounded buffer, so a compaction never holds the live set in memory (I-80).
// recs begin with a base record, so a crash that leaves the older segments replays them
// and then this one, which resets the inode and rebuilds the same state (I-397).
//
// Once the rename has happened the new segment is the log: offs is then returned even
// when a later step (directory fsync, reopen, removing an old segment) fails, with the
// error, and the WAL is left open on the new file whenever it can be opened at all.
func (w *WAL) Compact(recs []walRec) ([]int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return nil, w.failed
	}
	if w.syncing || w.synced != w.size { // the overlay quiesces appends first (I-168)
		return nil, errors.New("vfs: wal compact with an append in flight (bug)")
	}
	next := segOf(w.size) + 1
	final := w.segPath(next)
	tmp := final + ".new"
	nf, err := w.fs.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vfs: wal compact: %w", err)
	}
	abort := func(step string, err error) ([]int64, error) {
		_ = nf.Close()
		_ = w.fs.Remove(tmp)
		return nil, fmt.Errorf("vfs: wal compact %s: %w", step, err)
	}
	offs := make([]int64, len(recs))
	buf := make([]byte, compactBuf)
	var pos int64
	for i, r := range recs {
		hdr := encodeRecord(r.typ, r.off, nil)
		binary.LittleEndian.PutUint32(hdr[13:], uint32(r.n))
		crc := crc32.Checksum(hdr[4:], crcTable)
		dataPos := pos + recHeaderLen
		for copied := int64(0); copied < int64(r.n); {
			chunk := buf[:min(int64(len(buf)), int64(r.n)-copied)]
			if err := w.readLocked(chunk, r.src+copied); err != nil {
				return abort("read", err)
			}
			if _, err := nf.WriteAt(chunk, dataPos+copied); err != nil {
				return abort("write", err)
			}
			crc = crc32.Update(crc, crcTable, chunk)
			copied += int64(len(chunk))
		}
		binary.LittleEndian.PutUint32(hdr[0:], crc)
		if _, err := nf.WriteAt(hdr, pos); err != nil {
			return abort("write", err)
		}
		offs[i] = segPos(next, dataPos)
		pos = dataPos + int64(r.n)
	}
	if err := nf.Sync(); err != nil {
		return abort("fsync", err)
	}
	// Every handle is closed before the rename and the removals: Windows refuses to
	// replace, rename or remove a file that has an open handle. The data is already
	// fsynced, so the only window is between rename and reopen, which a crash recovers
	// from by replaying either set of segments.
	_ = nf.Close()
	w.closeReaderLocked()
	_ = w.f.Close()
	if err := w.fs.Rename(tmp, final); err != nil {
		_ = w.fs.Remove(tmp)
		reopened, rerr := w.fs.OpenFile(w.segPath(segOf(w.size)), os.O_RDWR, 0o600)
		if rerr != nil {
			w.failed = fmt.Errorf("%w: reopen after a failed compaction rename: %w", ErrWALClosed, rerr)
			return nil, fmt.Errorf("vfs: wal compact rename: %w (reopen: %w)", err, rerr)
		}
		w.f = reopened
		return nil, fmt.Errorf("vfs: wal compact rename: %w", err)
	}
	var first error
	if err := w.fs.SyncDir(filepath.Dir(w.path)); err != nil {
		first = fmt.Errorf("vfs: wal compact fsync dir: %w", err)
	}
	old := w.segs
	w.segs = []segment{{n: next}}
	w.size, w.synced = segPos(next, pos), segPos(next, pos)
	reopened, err := w.fs.OpenFile(final, os.O_RDWR, 0o600)
	if err != nil {
		// the new log is on disk but this process cannot reach it: every later call on
		// this inode fails with ErrWALClosed instead of using the closed handle (I-101)
		w.failed = fmt.Errorf("%w: reopen after compaction: %w", ErrWALClosed, err)
		if first == nil {
			first = w.failed
		}
		return nil, first
	}
	w.f = reopened
	for _, s := range old {
		if err := w.fs.Remove(w.segPath(s.n)); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = fmt.Errorf("vfs: wal compact remove old segment: %w", err)
		}
	}
	return offs, first
}

// readLocked is ReadAt with mu held.
func (w *WAL) readLocked(p []byte, walOff int64) error {
	n, off := segOf(walOff), segOff(walOff)
	if size := w.segSize(n); walOff < 0 || size < 0 || off+int64(len(p)) > size {
		return errWALShortRead
	}
	f, err := w.fileFor(n)
	if err != nil {
		return err
	}
	if _, err := f.ReadAt(p, off); err != nil {
		return fmt.Errorf("vfs: wal read: %w", err)
	}
	return nil
}

// walState is an inode's size and truncate state as a state or clean record carries it.
type walState struct {
	size, minSize uint64
	sizeSet       bool
}

// encodeState is a state record's data: minSize u64 | sizeSet u8 (the size is its off).
func encodeState(minSize uint64, sizeSet bool) []byte {
	b := make([]byte, 9)
	binary.LittleEndian.PutUint64(b, minSize)
	if sizeSet {
		b[8] = 1
	}
	return b
}

func decodeState(size uint64, data []byte) (walState, bool) {
	if len(data) != 9 || data[8] > 1 {
		return walState{}, false
	}
	return walState{size: size, minSize: binary.LittleEndian.Uint64(data), sizeSet: data[8] == 1}, true
}

// cleanRec is a decoded clean record: the extent pieces below log position w inside
// ranges were committed, and st is the inode's state after the Commit.
type cleanRec struct {
	w      int64
	st     walState
	ranges []fileRange
}

// encodeClean is a clean record's data: size u64 | minSize u64 | sizeSet u8 | count u32 |
// (start u64, end u64)… (W is its off).
func encodeClean(size, minSize uint64, sizeSet bool, ranges []fileRange) []byte {
	b := make([]byte, 0, 21+16*len(ranges))
	b = binary.LittleEndian.AppendUint64(b, size)
	b = append(b, encodeState(minSize, sizeSet)...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(ranges))) //nolint:gosec // a plan's chunks
	for _, r := range ranges {
		b = binary.LittleEndian.AppendUint64(b, r.start)
		b = binary.LittleEndian.AppendUint64(b, r.end)
	}
	return b
}

func decodeClean(w uint64, data []byte) (cleanRec, bool) {
	if len(data) < 21 || w > 1<<62 {
		return cleanRec{}, false
	}
	st, ok := decodeState(binary.LittleEndian.Uint64(data), data[8:17])
	if !ok {
		return cleanRec{}, false
	}
	n := binary.LittleEndian.Uint32(data[17:])
	if uint64(len(data)-21) != 16*uint64(n) {
		return cleanRec{}, false
	}
	c := cleanRec{w: int64(w), st: st, ranges: make([]fileRange, n)}
	for i := range c.ranges {
		at := 21 + 16*i
		c.ranges[i] = fileRange{binary.LittleEndian.Uint64(data[at:]), binary.LittleEndian.Uint64(data[at+8:])}
		if c.ranges[i].end <= c.ranges[i].start || (i > 0 && c.ranges[i].start < c.ranges[i-1].end) {
			return cleanRec{}, false
		}
	}
	return c, true
}
