// Package sysfs is the filesystem seam used by the shard store and nothing else. It exists
// so tests can inject ENOSPC, torn writes and crashes at each step of the
// temp → write → fsync → close → rename → dir-sync sequence. Production uses OS.
package sysfs

import (
	"io"
	"io/fs"
	"os"
	"time"
)

// FS is the minimal set of operations the shard store needs.
type FS interface {
	CreateTemp(dir, pattern string) (File, error)
	Open(name string) (File, error)
	Rename(oldpath, newpath string) error
	Remove(name string) error
	ReadDir(name string) ([]fs.DirEntry, error)
	MkdirAll(name string, perm fs.FileMode) error
	Stat(name string) (fs.FileInfo, error)
	Chtimes(name string, atime, mtime time.Time) error
	// SyncDir fsyncs a directory so a completed rename is durable. No-op on Windows.
	SyncDir(name string) error
}

// File is the subset of *os.File the shard store uses.
type File interface {
	io.Reader
	io.ReaderAt
	io.Writer
	io.Closer
	Sync() error
	Name() string
	Stat() (fs.FileInfo, error)
}

// OS is the real filesystem.
type OS struct{}

var _ FS = OS{}

func (OS) CreateTemp(dir, pattern string) (File, error) { return os.CreateTemp(dir, pattern) }
func (OS) Open(name string) (File, error)               { return os.Open(name) }
func (OS) Rename(oldpath, newpath string) error         { return os.Rename(oldpath, newpath) }
func (OS) Remove(name string) error                     { return os.Remove(name) }
func (OS) ReadDir(name string) ([]fs.DirEntry, error)   { return os.ReadDir(name) }
func (OS) MkdirAll(name string, perm fs.FileMode) error { return os.MkdirAll(name, perm) }
func (OS) Stat(name string) (fs.FileInfo, error)        { return os.Stat(name) }
func (OS) Chtimes(name string, atime, mtime time.Time) error {
	return os.Chtimes(name, atime, mtime)
}
func (OS) SyncDir(name string) error { return syncDir(name) }
