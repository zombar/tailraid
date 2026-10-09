package smb_test

import (
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// stubFS is a filesystem with nothing in it, for the tests that are about the
// handshake rather than about files.
type stubFS struct{}

func (stubFS) ReadFile(string) ([]byte, error)               { return nil, os.ErrNotExist }
func (stubFS) WriteFile(string, []byte, os.FileMode) error   { return os.ErrPermission }
func (stubFS) ListDir(string) ([]filesystem.DirEntry, error) { return nil, nil }
func (stubFS) MkDir(string, os.FileMode) error               { return os.ErrPermission }
func (stubFS) Stat(string) (filesystem.Stat, error)          { return nil, os.ErrNotExist }
func (stubFS) DeleteFile(string) error                       { return os.ErrPermission }
func (stubFS) DeleteDir(string) error                        { return os.ErrPermission }
func (stubFS) Rename(string, string) error                   { return os.ErrPermission }
func (stubFS) Truncate(string, int64) error                  { return os.ErrPermission }
func (stubFS) Symlink(string, string) error                  { return os.ErrPermission }
func (stubFS) ReadLink(string) (string, error)               { return "", os.ErrNotExist }
func (stubFS) Label() string                                 { return "STUB" }
func (stubFS) Close() error                                  { return nil }
