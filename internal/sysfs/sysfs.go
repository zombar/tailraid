// Package sysfs is the filesystem seam used by the shard store and the per-inode WAL and
// nothing else. It exists so tests can inject ENOSPC, torn writes, failed fsyncs and
// crashes at each step of the temp → write → fsync → close → rename → dir-sync sequence.
// Production uses OS.
package sysfs

import (
	"io"
	"io/fs"
	"os"
	"time"
)

// FS is the minimal set of operations the shard store and the WAL need.
type FS interface {
	CreateTemp(dir, pattern string) (File, error)
	Open(name string) (File, error)
	// OpenFile is os.OpenFile; the WAL uses it to create or reopen its log read-write.
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	Rename(oldpath, newpath string) error
	Remove(name string) error
	ReadDir(name string) ([]fs.DirEntry, error)
	MkdirAll(name string, perm fs.FileMode) error
	Stat(name string) (fs.FileInfo, error)
	Chtimes(name string, atime, mtime time.Time) error
	// SyncDir fsyncs a directory so a completed rename is durable. No-op on Windows.
	SyncDir(name string) error
}

// File is the subset of *os.File the shard store and the WAL use.
type File interface {
	io.Reader
	io.ReaderAt
	io.Writer
	io.WriterAt
	io.Closer
	Sync() error
	Name() string
	Stat() (fs.FileInfo, error)
	Truncate(size int64) error
}

// OS is the real filesystem.
type OS struct{}

var _ FS = OS{}

func (OS) CreateTemp(dir, pattern string) (File, error) { return os.CreateTemp(dir, pattern) }
func (OS) Open(name string) (File, error)               { return os.Open(name) }
func (OS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	return os.OpenFile(name, flag, perm)
}
func (OS) Rename(oldpath, newpath string) error         { return os.Rename(oldpath, newpath) }
func (OS) Remove(name string) error                     { return os.Remove(name) }
func (OS) ReadDir(name string) ([]fs.DirEntry, error)   { return os.ReadDir(name) }
func (OS) MkdirAll(name string, perm fs.FileMode) error { return os.MkdirAll(name, perm) }
func (OS) Stat(name string) (fs.FileInfo, error)        { return os.Stat(name) }
func (OS) Chtimes(name string, atime, mtime time.Time) error {
	return os.Chtimes(name, atime, mtime)
}
func (OS) SyncDir(name string) error { return syncDir(name) }
