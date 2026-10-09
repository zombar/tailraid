package vfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"

	"github.com/zombar/tailraid/internal/sysfs"
)

// WAL record types.
const (
	recWrite    uint8 = 1 // off = file offset, data = bytes
	recTruncate uint8 = 2 // off = new size, no data
	recBase     uint8 = 3 // off = committed size the following records apply to, no data
)

// Record layout: crc32c(u32 LE, over everything after it) | type u8 | off u64 LE | len u32 LE | data.
const recHeaderLen = 4 + 1 + 8 + 4

// maxRecordData bounds a single record so a corrupt length can never allocate unboundedly.
const maxRecordData = 64 << 20

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var errWALShortRead = errors.New("vfs: wal read beyond end")

// ErrWALClosed is every call on a WAL whose file could not be reopened after a failed
// fsync or compaction (I-101): the inode's writes fail with it until the gateway restarts
// and replays the log, rather than touching a handle that is closed or stale.
var ErrWALClosed = errors.New("vfs: wal unusable until restart")

// walRec is a record to be written by Compact: a write record names its bytes by their
// offset and length in the log being compacted, so nothing is read ahead of the copy.
type walRec struct {
	typ uint8
	off uint64
	src int64  // data offset in the current log (recWrite)
	n   uint64 // data length (recWrite)
}

// compactBuf bounds the copy of one record during a compaction (docs/04 §2, I-80).
const compactBuf = 1 << 20

// WAL is a per-inode append-only log. Every append is fsynced before it returns, so a
// successful append means the bytes survive a crash or power loss (I-7). All file access
// goes through the sysfs.FS seam so tests can inject fsync failures and crashes.
type WAL struct {
	fs     sysfs.FS
	path   string
	f      sysfs.File
	size   int64
	failed error // ErrWALClosed with its cause, once the file could not be reopened
	// Discarded is the number of bytes Replay cut off after the last intact record: a
	// torn tail from a crash, or everything after a record that failed its CRC.
	Discarded int64
}

func openWAL(fsys sysfs.FS, path string) (*WAL, error) {
	f, err := fsys.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vfs: open wal: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vfs: stat wal: %w", err)
	}
	if st.Size() == 0 {
		// A new (or still empty) log: its directory entry must be durable before any
		// record it holds is acknowledged (I-63).
		if err := fsys.SyncDir(filepath.Dir(path)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("vfs: fsync wal dir: %w", err)
		}
	}
	return &WAL{fs: fsys, path: path, f: f, size: st.Size()}, nil
}

// Size is the byte length of the log.
func (w *WAL) Size() int64 { return w.size }

func (w *WAL) Close() error { return w.f.Close() }

func encodeRecord(typ uint8, off uint64, data []byte) []byte {
	buf := make([]byte, recHeaderLen+len(data))
	buf[4] = typ
	binary.LittleEndian.PutUint64(buf[5:], off)
	binary.LittleEndian.PutUint32(buf[13:], uint32(len(data)))
	copy(buf[recHeaderLen:], data)
	binary.LittleEndian.PutUint32(buf[0:], crc32.Checksum(buf[4:], crcTable))
	return buf
}

// append writes one record at the end and fsyncs; returns the offset of its data.
func (w *WAL) append(typ uint8, off uint64, data []byte) (int64, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if len(data) > maxRecordData {
		return 0, fmt.Errorf("vfs: wal record too large (%d bytes)", len(data))
	}
	buf := encodeRecord(typ, off, data)
	if _, err := w.f.WriteAt(buf, w.size); err != nil {
		return 0, fmt.Errorf("vfs: wal write: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		// Not acknowledged: cut the record back off so a crash cannot replay it (I-37),
		// then reopen the file: after a failed fsync the kernel may have dropped the
		// dirty pages and cleared the error on this descriptor, so the next fsync on it
		// would report success for data that never reached the disk (I-101).
		_ = w.f.Truncate(w.size)
		_ = w.f.Sync()
		_ = w.f.Close()
		if f, rerr := w.fs.OpenFile(w.path, os.O_RDWR, 0o600); rerr == nil {
			w.f = f
		} else {
			w.failed = fmt.Errorf("%w: reopen after a failed fsync: %w", ErrWALClosed, rerr)
		}
		return 0, fmt.Errorf("vfs: wal fsync: %w", err)
	}
	dataOff := w.size + recHeaderLen
	w.size += int64(len(buf))
	return dataOff, nil
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

// ReadAt fills p with logged bytes starting at walOff (a data offset returned by AppendWrite,
// possibly advanced into the record by an extent split).
func (w *WAL) ReadAt(p []byte, walOff int64) error {
	if w.failed != nil {
		return w.failed
	}
	if walOff < 0 || walOff+int64(len(p)) > w.size {
		return errWALShortRead
	}
	if _, err := w.f.ReadAt(p, walOff); err != nil {
		return fmt.Errorf("vfs: wal read: %w", err)
	}
	return nil
}

// Replay calls fn for every intact record in order. It stops silently at the first record
// whose length or CRC does not check out (a torn tail from a crash mid-append) and
// truncates the log to the last intact record (I-37).
func (w *WAL) Replay(fn func(typ uint8, off uint64, n uint32, walOff int64, data []byte) error) error {
	var pos int64
	hdr := make([]byte, recHeaderLen)
	for pos+recHeaderLen <= w.size {
		if _, err := w.f.ReadAt(hdr, pos); err != nil {
			return fmt.Errorf("vfs: wal replay: %w", err)
		}
		n := binary.LittleEndian.Uint32(hdr[13:])
		if n > maxRecordData || pos+recHeaderLen+int64(n) > w.size {
			break
		}
		data := make([]byte, n)
		if n > 0 {
			if _, err := w.f.ReadAt(data, pos+recHeaderLen); err != nil {
				return fmt.Errorf("vfs: wal replay: %w", err)
			}
		}
		crc := crc32.Update(crc32.Checksum(hdr[4:], crcTable), crcTable, data)
		if crc != binary.LittleEndian.Uint32(hdr[0:]) {
			break
		}
		typ := hdr[4]
		off := binary.LittleEndian.Uint64(hdr[5:])
		if err := fn(typ, off, n, pos+recHeaderLen, data); err != nil {
			return err
		}
		pos += recHeaderLen + int64(n)
	}
	if pos < w.size {
		w.Discarded = w.size - pos
		if err := w.f.Truncate(pos); err != nil {
			return fmt.Errorf("vfs: wal truncate torn tail: %w", err)
		}
		if err := w.f.Sync(); err != nil {
			return fmt.Errorf("vfs: wal fsync: %w", err)
		}
		w.size = pos
	}
	return nil
}

// Compact atomically replaces the log with exactly recs, returning each record's data
// offset in the new log. A write record's bytes are streamed from the current log
// through one bounded buffer, so a compaction never holds the live set in memory (I-80).
//
// Once the rename has happened the new log is the log: offs is then returned even when a
// later step (directory fsync, reopen) fails, with the error, and the WAL is left open on
// the new file whenever it can be opened at all.
func (w *WAL) Compact(recs []walRec) ([]int64, error) {
	if w.failed != nil {
		return nil, w.failed
	}
	tmp := w.path + ".new"
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
			if err := w.ReadAt(chunk, r.src+copied); err != nil {
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
		offs[i] = dataPos
		pos = dataPos + int64(r.n)
	}
	if err := nf.Sync(); err != nil {
		return abort("fsync", err)
	}
	// Both handles are closed before the rename: Windows refuses to replace or rename a
	// file that has an open handle. The data is already fsynced, so the only window is
	// between rename and reopen, which a crash recovers from by replaying either file.
	_ = nf.Close()
	_ = w.f.Close()
	if err := w.fs.Rename(tmp, w.path); err != nil {
		_ = w.fs.Remove(tmp)
		reopened, rerr := w.fs.OpenFile(w.path, os.O_RDWR, 0o600)
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
	reopened, err := w.fs.OpenFile(w.path, os.O_RDWR, 0o600)
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
	w.size = pos
	return offs, first
}
