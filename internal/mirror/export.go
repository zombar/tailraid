package mirror

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// Resolve finds the entry at path p in the snapshot whose root tree is root; the root
// itself is a directory entry with Child == root (docs/16 §6).
func (m *Mirror) Resolve(root meta.Hash, p string) (Entry, error) {
	e := Entry{Type: TypeDir, Child: root, Mode: 0o755}
	for _, name := range meta.SplitPath(p) {
		if e.Type != TypeDir {
			return Entry{}, fmt.Errorf("%w: %s is not a directory", ErrNotFound, e.Name)
		}
		t, err := m.Tree(e.Child)
		if err != nil {
			return Entry{}, err
		}
		next, ok := t.Lookup(name)
		if !ok {
			return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		e = next
	}
	return e, nil
}

// ReadFile writes file entry e's bytes to w: its chunks in order, a hole and the tail of
// a chunk shorter than its slot as zeros, cut at Size (I-1, the volume's own rule).
func (m *Mirror) ReadFile(e Entry, chunkSize uint32, w io.Writer) error {
	if e.Type != TypeFile {
		return fmt.Errorf("mirror: %s is not a file", e.Name)
	}
	cs := uint64(chunkSize)
	left := e.Size
	for _, h := range e.Chunks {
		if left == 0 {
			break
		}
		slot := min(cs, left)
		var data []byte
		if h != meta.HashZero {
			var err error
			if data, err = m.Object(h); err != nil {
				return err
			}
		}
		if uint64(len(data)) > slot {
			data = data[:slot]
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		if pad := slot - uint64(len(data)); pad > 0 {
			if _, err := io.CopyN(w, zeroReader{}, int64(pad)); err != nil {
				return err
			}
		}
		left -= slot
	}
	if left > 0 {
		_, err := io.CopyN(w, zeroReader{}, int64(left)) // a size past the last chunk reads as zeros
		return err
	}
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Export copies the part of snapshot man at path p into the local directory dst, with
// modes and modification times as recorded (owners are left to the user running it):
// the standalone reader of docs/16 §7. A file that cannot be read whole (a missing or
// corrupt object) is reported and skipped; every other entry is still written.
func (m *Mirror) Export(man Manifest, p, dst string) error {
	e, err := m.Resolve(meta.Hash(man.Root), p)
	if err != nil {
		return err
	}
	if e.Name == "" {
		e.Name = filepath.Base(filepath.Clean(dst))
		return m.exportDir(e, man.ChunkSize, dst)
	}
	return m.export(e, man.ChunkSize, filepath.Join(dst, e.Name))
}

func (m *Mirror) exportDir(e Entry, cs uint32, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	t, err := m.Tree(e.Child)
	if err != nil {
		return err
	}
	var errs []error
	for i := range t.Entries {
		c := &t.Entries[i]
		if err := m.export(*c, cs, filepath.Join(dst, c.Name)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.Chmod(dst, os.FileMode(e.Mode&0o777)|0o700); err != nil {
		errs = append(errs, err)
	}
	if e.Mtime != 0 {
		mt := time.Unix(0, e.Mtime)
		errs = append(errs, os.Chtimes(dst, mt, mt))
	}
	return errors.Join(errs...)
}

func (m *Mirror) export(e Entry, cs uint32, dst string) error {
	switch e.Type {
	case TypeDir:
		return m.exportDir(e, cs, dst)
	case TypeSymlink:
		return os.Symlink(e.Target, dst)
	case TypeFile:
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the user's own export target
		if err != nil {
			return err
		}
		if err := m.ReadFile(e, cs, f); err != nil {
			_ = f.Close()
			_ = os.Remove(dst)
			return fmt.Errorf("%s: %w", dst, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Chmod(dst, os.FileMode(e.Mode&0o777)); err != nil {
			return err
		}
		mt := time.Unix(0, e.Mtime)
		return os.Chtimes(dst, mt, mt)
	}
	return fmt.Errorf("mirror: %s has type %d", dst, e.Type)
}
