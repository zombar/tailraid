package smb_test

import (
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// memFS is a filesystem in a map, for tests about the protocol rather than
// about a driver. It comes in two shapes on purpose: with Opener/WritableFile,
// which is the path a real driver takes, and without, which is the whole-file
// fallback every driver that lacks them uses. Both must serve the same mount.
// memFS has NO lock of its own, deliberately.
//
// It had one, and that is why the race lane stayed green while the server
// handed a single Filesystem to every connection at once: the test's driver
// was doing the serialising the server was not. A Filesystem promises nothing
// about concurrent path-based calls, so a driver without a lock is the honest
// one to test against -- and TestSeveralClientsOneShare will find any command
// that forgot to take the share's.
type memFS struct {
	files      map[string][]byte
	dirs       map[string]bool
	positional bool
}

func newMemFS(positional bool) *memFS {
	return &memFS{
		files:      map[string][]byte{},
		dirs:       map[string]bool{"/": true},
		positional: positional,
	}
}

func (m *memFS) Close() error  { return nil }
func (m *memFS) Label() string { return "MEMFS" }

func (m *memFS) ReadFile(p string) ([]byte, error) {
	b, ok := m.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}

func (m *memFS) WriteFile(p string, data []byte, _ os.FileMode) error {
	if !m.dirs[path.Dir(p)] {
		return os.ErrNotExist
	}
	m.files[p] = append([]byte(nil), data...)
	return nil
}

func (m *memFS) ListDir(p string) ([]filesystem.DirEntry, error) {
	if !m.dirs[p] {
		return nil, os.ErrNotExist
	}
	var names []string
	for f := range m.files {
		if path.Dir(f) == p {
			names = append(names, path.Base(f))
		}
	}
	for d := range m.dirs {
		if d != "/" && path.Dir(d) == p {
			names = append(names, path.Base(d))
		}
	}
	sort.Strings(names)
	out := make([]filesystem.DirEntry, 0, len(names))
	for i, n := range names {
		t := uint8(1)
		if m.dirs[path.Join(p, n)] {
			t = 2
		}
		out = append(out, filesystem.NewDirEntry(uint64(i+2), n, t))
	}
	return out, nil
}

func (m *memFS) Stat(p string) (filesystem.Stat, error) {
	if m.dirs[p] {
		return filesystem.NewStat(0o040755, 0, 1), nil
	}
	if b, ok := m.files[p]; ok {
		return filesystem.NewStat(0o100644, uint64(len(b)), 2), nil
	}
	return nil, os.ErrNotExist
}

func (m *memFS) MkDir(p string, _ os.FileMode) error {
	if !m.dirs[path.Dir(p)] {
		return os.ErrNotExist
	}
	if m.dirs[p] {
		return os.ErrExist
	}
	m.dirs[p] = true
	return nil
}

func (m *memFS) DeleteFile(p string) error {
	if _, ok := m.files[p]; !ok {
		return os.ErrNotExist
	}
	delete(m.files, p)
	return nil
}

func (m *memFS) DeleteDir(p string) error {
	if !m.dirs[p] {
		return os.ErrNotExist
	}
	for f := range m.files {
		if strings.HasPrefix(f, p+"/") {
			return errors.New("directory not empty")
		}
	}
	delete(m.dirs, p)
	return nil
}

func (m *memFS) Rename(from, to string) error {
	b, ok := m.files[from]
	if !ok {
		return os.ErrNotExist
	}
	delete(m.files, from)
	m.files[to] = b
	return nil
}

func (m *memFS) ReadLink(string) (string, error) { return "", os.ErrInvalid }

// OpenFile is the optional capability, and memFS answers it only when it was
// built to: a test that wants the fallback asks for a filesystem without it.
func (m *memFS) OpenFile(p string) (filesystem.File, error) {
	if !m.positional {
		return nil, errors.New("this filesystem has no positional access")
	}
	if m.dirs[p] {
		return nil, errors.New("that is a directory")
	}
	if _, ok := m.files[p]; !ok {
		return nil, os.ErrNotExist
	}
	return &memFile{fs: m, path: p}, nil
}

type memFile struct {
	fs   *memFS
	path string
}

func (f *memFile) Close() error { return nil }

func (f *memFile) Size() int64 {
	return int64(len(f.fs.files[f.path]))
}

// ReadAt follows io.ReaderAt to the letter, because that is what the capability
// promises and what a mount serving parallel requests relies on.
func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	b := f.fs.files[f.path]
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	b := f.fs.files[f.path]
	if end := off + int64(len(p)); int64(len(b)) < end {
		grown := make([]byte, end)
		copy(grown, b)
		b = grown
	}
	n := copy(b[off:], p)
	f.fs.files[f.path] = b
	return n, nil
}

func (f *memFile) Truncate(n int64) error {
	b := f.fs.files[f.path]
	if int64(len(b)) > n {
		b = b[:n]
	} else {
		grown := make([]byte, n)
		copy(grown, b)
		b = grown
	}
	f.fs.files[f.path] = b
	return nil
}

func (f *memFile) Sync() error { return nil }
