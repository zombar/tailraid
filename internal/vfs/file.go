package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/zombar/tailraid/internal/meta"
)

// File is an open handle on an inode. go-nfs opens, seeks, writes and closes a File for
// every WRITE RPC, so a File carries no buffered state: writes go straight to the overlay
// WAL and reads merge the overlay over committed chunks.
type File struct {
	fs     *FS
	ino    meta.Ino
	name   string
	flag   int
	pos    int64
	closed bool
	snap   *snapFile // a file under /.snapshots: read from its snapshot, never written (I-185)
}

func (f *File) checkOpen(op string) error {
	if f.closed {
		return mapErr(op, f.name, os.ErrClosed)
	}
	return nil
}

var _ billy.File = (*File)(nil)

func (f *File) Name() string { return f.name }

func (f *File) inode() (meta.Inode, error) {
	in, ok := f.fs.st.GetInodeAny(f.ino) // an open file in the trash still reads and flushes (I-155)
	if !ok {
		return meta.Inode{}, mapErr("read", f.name, meta.ErrNotExist)
	}
	return in, nil
}

// size is the client-visible size: the overlay's while dirty, else the committed size.
func (f *File) size() uint64 {
	if f.snap != nil {
		return f.snap.e.Size
	}
	in, ok := f.fs.st.GetInodeAny(f.ino) // an open file in the trash still reads and flushes (I-155)
	if !ok {
		return 0
	}
	if sz, ok := f.fs.ov.Size(f.ino, in.Size); ok {
		return sz
	}
	return in.Size
}

func (f *File) writable() bool { return f.flag&(os.O_WRONLY|os.O_RDWR) != 0 }

// Write writes at the current position and advances it: the NFS gateway's seek-then-write
// per RPC (docs/07).
func (f *File) Write(p []byte) (int, error) {
	n, err := f.WriteAt(p, f.pos)
	f.pos += int64(n)
	return n, err
}

// WriteAt writes p at off and leaves the position alone: the SMB gateway's WRITE, which
// names its offset (docs/15). The bytes are in the fsynced WAL when it returns (I-7).
func (f *File) WriteAt(p []byte, off int64) (int, error) {
	if err := f.checkOpen("write"); err != nil {
		return 0, err
	}
	if !f.writable() {
		return 0, mapErr("write", f.name, syscall.EBADF)
	}
	if off < 0 {
		return 0, mapErr("write", f.name, syscall.EINVAL)
	}
	in, err := f.inode()
	if err != nil {
		return 0, err
	}
	if err := f.fs.ov.Write(f.ino, in.Size, uint64(off), p); err != nil {
		return 0, mapErr("write", f.name, err)
	}
	f.fs.bytesWritten.Add(uint64(len(p)))
	return len(p), nil
}

func (f *File) Read(p []byte) (int, error) {
	if err := f.checkOpen("read"); err != nil {
		return 0, err
	}
	n, err := f.ReadAt(p, f.pos)
	f.pos += int64(n)
	return n, err
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if err := f.checkOpen("read"); err != nil {
		return 0, err
	}
	if f.flag&os.O_WRONLY != 0 {
		return 0, mapErr("read", f.name, syscall.EBADF)
	}
	if off < 0 {
		return 0, mapErr("read", f.name, syscall.EINVAL)
	}
	if f.snap != nil {
		n, err := f.fs.snapReadAt(context.Background(), f.snap, p, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, snapErr("read", f.name, err)
		}
		return n, err
	}
	in, err := f.inode()
	if err != nil {
		return 0, err
	}
	// readahead for a sequential client (docs/04 §5, I-166) starts once this read is
	// served, so the chunk the client waits for never competes with its prefetches (I-203)
	defer f.fs.rd.Prefetch(in, off)
	if _, dirty := f.fs.ov.Size(f.ino, in.Size); dirty {
		n, err := f.fs.ov.Read(f.ino, in.Size, uint64(off), p, f.fs.rd.BaseReaderEarly(context.Background(), in))
		if err == nil || errors.Is(err, io.EOF) {
			f.fs.bytesRead.Add(uint64(max(n, 0))) //nolint:gosec // n is a count
			return n, err
		}
		if !errors.Is(err, meta.ErrNotExist) { // the flush committed and evicted the entry in between: the base has it all
			return n, mapErr("read", f.name, err)
		}
	}
	n, err := f.fs.rd.ReadAtEarly(context.Background(), in, p, off) // a range from one verified shard when it can (I-202)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, mapErr("read", f.name, err)
	}
	f.fs.bytesRead.Add(uint64(max(n, 0))) //nolint:gosec // n is a count
	return n, err
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	if err := f.checkOpen("seek"); err != nil {
		return 0, err
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.pos + offset
	case io.SeekEnd:
		abs = int64(f.size()) + offset
	default:
		return 0, mapErr("seek", f.name, syscall.EINVAL)
	}
	if abs < 0 {
		return 0, mapErr("seek", f.name, syscall.EINVAL)
	}
	f.pos = abs
	return abs, nil
}

func (f *File) Truncate(size int64) error {
	if f.snap != nil {
		return mapErr("truncate", f.name, ErrReadOnly)
	}
	if !f.writable() || size < 0 {
		return mapErr("truncate", f.name, syscall.EINVAL)
	}
	in, err := f.inode()
	if err != nil {
		return err
	}
	if err := f.fs.ov.Truncate(f.ino, in.Size, uint64(size)); err != nil {
		return mapErr("truncate", f.name, err)
	}
	return nil
}

// Close only marks the handle: durability is the WAL fsync that already happened in Write.
func (f *File) Close() error {
	if f.closed {
		return mapErr("close", f.name, os.ErrClosed)
	}
	f.closed = true
	return nil
}

func (f *File) Lock() error   { return nil }
func (f *File) Unlock() error { return nil }

// fileInfo is the os.FileInfo for an inode; Sys() exposes the inode number as the NFS
// fileid (docs/07) so handles and READDIRPLUS agree.
type fileInfo struct {
	in    meta.Inode
	size  uint64
	dirty bool
}

var _ os.FileInfo = (*fileInfo)(nil)

func (fi *fileInfo) Name() string { return fi.in.Name }
func (fi *fileInfo) Size() int64  { return int64(fi.size) }
func (fi *fileInfo) Mode() os.FileMode {
	m := os.FileMode(fi.in.Mode & 0o777)
	switch fi.in.Type {
	case meta.TDir:
		m |= os.ModeDir
	case meta.TSymlink:
		m |= os.ModeSymlink
	case meta.TFile:
	}
	return m
}
func (fi *fileInfo) ModTime() time.Time { return time.Unix(0, fi.in.Mtime) }

// Dirty reports acknowledged bytes not yet committed to the cluster (the size and the
// bytes come from the overlay; docs/13 omits the ETag while it is set).
func (fi *fileInfo) Dirty() bool { return fi.dirty }

// Ino and Times give a gateway that needs more than os.FileInfo (the SMB adapter,
// docs/15) the inode number and the inode's own timestamps, the way Dirty serves the web
// UI; both are read through small optional interfaces.
func (fi *fileInfo) Ino() meta.Ino { return fi.in.Ino }

func (fi *fileInfo) Times() (atime, mtime, ctime time.Time) {
	return time.Unix(0, fi.in.Atime), time.Unix(0, fi.in.Mtime), time.Unix(0, fi.in.Ctime)
}
func (fi *fileInfo) IsDir() bool { return fi.in.Type == meta.TDir }
func (fi *fileInfo) Sys() any {
	return &nfsfile.FileInfo{Nlink: uint32(fi.in.Nlink), UID: fi.in.Uid, GID: fi.in.Gid, Fileid: uint64(fi.in.Ino)}
}
