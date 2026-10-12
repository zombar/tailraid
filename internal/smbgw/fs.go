// Package smbgw is the SMB2 gateway (docs/15): github.com/go-filesystems/smb serving the
// shared vfs through an adapter to its backend contract. Windows mounts it; macOS and
// Linux keep the NFS gateway (docs/07).
package smbgw

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	filesystem "github.com/go-filesystems/interface"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/vfs"
)

// ShareName and User are the one share and the one account the gateway exports; only the
// password is per device (docs/15 §6).
const (
	ShareName = "tailraid"
	User      = "tailraid"
)

// FS adapts a vfs.FS to the go-filesystems backend contract: the required Filesystem plus
// the positional, truncate, symlink and metadata capabilities the server probes for.
type FS struct {
	fs *vfs.FS
	st *meta.Store
}

var (
	_ filesystem.Filesystem     = (*FS)(nil)
	_ filesystem.Opener         = (*FS)(nil)
	_ filesystem.Truncater      = (*FS)(nil)
	_ filesystem.Symlinker      = (*FS)(nil)
	_ filesystem.MetadataSetter = (*FS)(nil)
	_ filesystem.WritableFile   = (*file)(nil)
)

// New wraps fs; st answers the share's size (meta.Usage, as NFS FSSTAT and status do).
func New(fs *vfs.FS, st *meta.Store) *FS { return &FS{fs: fs, st: st} }

// Capacity is the share's total and free bytes: meta.Usage over the node table.
func (a *FS) Capacity() (total, avail uint64) {
	total, _, free := meta.Usage(a.st.Volume(), a.st.Nodes(), a.st.Stats())
	return total, free
}

// wrap turns a vfs error into one the server classifies (docs/15 §4). ENOENT, EEXIST,
// ENOSPC and EFBIG already satisfy the errors it recognises; EROFS (the stale gateway,
// docs/02) does not, and is marked as a permission error so it reaches the wire as
// STATUS_ACCESS_DENIED rather than an operation's default.
func wrap(err error) error {
	if err != nil && errors.Is(err, vfs.ErrReadOnly) {
		return fmt.Errorf("%w: %w", os.ErrPermission, err)
	}
	return err
}

// CanDelete is asked when a client marks a handle delete-on-close (docs/15 §8): the
// delete itself happens at CLOSE, whose reply carries no failure, so a protected entry
// (I-154) is refused here, where the client reads STATUS_ACCESS_DENIED.
func (f *FS) CanDelete(p string) error {
	if parts := meta.SplitPath(p); len(parts) > 0 && parts[0] == vfs.SnapshotDir {
		return os.ErrPermission // read-only (I-185)
	}
	in, err := f.st.Resolve(meta.SplitPath(p))
	if err != nil {
		return os.ErrNotExist
	}
	if f.st.Protected(in.Ino) {
		return os.ErrPermission
	}
	return nil
}

// POSIX type bits of a Stat mode, which is what the server reads.
const (
	modeDir     uint16 = 0o040000
	modeRegular uint16 = 0o100000
	modeSymlink uint16 = 0o120000
)

// DirEntry file types (the d_type convention; the server stats every entry and reads
// none of them).
const (
	typeDir     uint8 = 4
	typeRegular uint8 = 8
	typeSymlink uint8 = 10
)

// stat is a filesystem.Stat that also carries the inode's own times for a server that
// asks for them (docs/15 §8).
type stat struct {
	mode  uint16
	size  uint64
	ino   uint64
	atime time.Time
	mtime time.Time
	ctime time.Time
}

func (s *stat) Mode() uint16  { return s.mode }
func (s *stat) Size() uint64  { return s.size }
func (s *stat) Inode() uint64 { return s.ino }

func (s *stat) Times() (atime, mtime, ctime time.Time) { return s.atime, s.mtime, s.ctime }

func statOf(fi os.FileInfo) *stat {
	st := &stat{size: uint64(max(fi.Size(), 0)), mode: uint16(fi.Mode().Perm())}
	switch {
	case fi.IsDir():
		st.mode |= modeDir
	case fi.Mode()&os.ModeSymlink != 0:
		st.mode |= modeSymlink
	default:
		st.mode |= modeRegular
	}
	if in, ok := fi.(interface{ Ino() meta.Ino }); ok {
		st.ino = uint64(in.Ino())
	}
	if tm, ok := fi.(interface {
		Times() (atime, mtime, ctime time.Time)
	}); ok {
		st.atime, st.mtime, st.ctime = tm.Times()
	} else {
		st.atime, st.mtime, st.ctime = fi.ModTime(), fi.ModTime(), fi.ModTime()
	}
	return st
}

func typeOf(fi os.FileInfo) uint8 {
	switch {
	case fi.IsDir():
		return typeDir
	case fi.Mode()&os.ModeSymlink != 0:
		return typeSymlink
	}
	return typeRegular
}

// --- filesystem.Filesystem ---------------------------------------------------------------

func (a *FS) Close() error { return nil }

func (a *FS) Stat(path string) (filesystem.Stat, error) {
	fi, err := a.fs.Lstat(path)
	if err != nil {
		return nil, wrap(err)
	}
	return statOf(fi), nil
}

func (a *FS) ListDir(path string) ([]filesystem.DirEntry, error) {
	fis, err := a.fs.ReadDir(path)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]filesystem.DirEntry, 0, len(fis))
	for _, fi := range fis {
		out = append(out, filesystem.NewDirEntry(statOf(fi).ino, fi.Name(), typeOf(fi)))
	}
	return out, nil
}

// ReadFile is the whole-file fallback the server uses only when OpenFile fails.
func (a *FS) ReadFile(path string) ([]byte, error) {
	f, err := a.fs.Open(path)
	if err != nil {
		return nil, wrap(err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	return b, wrap(err)
}

// WriteFile is the server's CREATE, overwrite and supersede: the file is created or
// emptied, then holds data (nil for a fresh file).
func (a *FS) WriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := a.fs.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return wrap(err)
	}
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return wrap(err)
		}
	}
	return wrap(f.Close())
}

func (a *FS) ReadLink(path string) (string, error) {
	target, err := a.fs.Readlink(path)
	return target, wrap(err)
}

func (a *FS) MkDir(path string, perm os.FileMode) error { return wrap(a.fs.MkdirAll(path, perm)) }

func (a *FS) DeleteFile(path string) error { return wrap(a.fs.Remove(path)) }

func (a *FS) DeleteDir(path string) error { return wrap(a.fs.Remove(path)) }

func (a *FS) Rename(oldPath, newPath string) error { return wrap(a.fs.Rename(oldPath, newPath)) }

// --- optional capabilities ---------------------------------------------------------------

// OpenFile is the positional handle the server prefers: every READ and WRITE goes through
// it rather than the whole-file calls.
func (a *FS) OpenFile(path string) (filesystem.File, error) {
	f, err := a.fs.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, vfs.ErrReadOnly) {
		f, err = a.fs.OpenFile(path, os.O_RDONLY, 0) // a snapshot reads; its writes are refused (I-185)
	}
	if err != nil {
		return nil, wrap(err)
	}
	vf, ok := f.(*vfs.File)
	if !ok {
		_ = f.Close()
		return nil, fmt.Errorf("smbgw: %s: not a vfs file", path)
	}
	return &file{f: vf}, nil
}

func (a *FS) Truncate(path string, size int64) error {
	f, err := a.fs.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return wrap(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return wrap(err)
	}
	return wrap(f.Close())
}

func (a *FS) Symlink(target, linkPath string) error { return wrap(a.fs.Symlink(target, linkPath)) }

func (a *FS) Chmod(path string, perm os.FileMode) error { return wrap(a.fs.Chmod(path, perm)) }

func (a *FS) Chown(path string, uid, gid uint32) error {
	return wrap(a.fs.Lchown(path, int(uid), int(gid)))
}

func (a *FS) Chtimes(path string, atime, mtime time.Time) error {
	return wrap(a.fs.Chtimes(path, atime, mtime))
}

// file is a filesystem.WritableFile over a vfs handle. WriteAt is durable in the WAL when
// it returns (I-7), which is why Sync has nothing left to do.
type file struct{ f *vfs.File }

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.f.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, wrap(err)
	}
	return n, err
}

func (f *file) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.f.WriteAt(p, off)
	return n, wrap(err)
}

func (f *file) Truncate(size int64) error {
	if size < 0 {
		return syscall.EINVAL
	}
	return wrap(f.f.Truncate(size))
}

func (f *file) Sync() error { return nil }

// Size is the client-visible length: the overlay's while dirty, else the committed size.
func (f *file) Size() int64 {
	n, err := f.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0
	}
	return n
}

func (f *file) Close() error { return wrap(f.f.Close()) }
