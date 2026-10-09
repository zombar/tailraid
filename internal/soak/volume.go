// Package soak runs the scenario sequence of docs/09 §6 against an Env (compose, live or
// the in-process harness) and renders the report.
package soak

import (
	"crypto/sha256"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/go-git/go-billy/v5"
)

// Volume is file IO through one gateway. Paths are volume-absolute ("/a/b").
type Volume interface {
	Mkdir(p string) error // MkdirAll semantics
	WriteAt(p string, off int64, data []byte) error
	ReadAll(p string) ([]byte, error)
	Truncate(p string, size int64) error
	Rename(from, to string) error
	Remove(p string) error // files and empty directories
	Size(p string) (int64, error)
	List(p string) ([]string, error) // entry names of a directory
	// Hash streams the file: its sha256 and size, so a large file is checked without
	// the driver holding it (docs/09 §6).
	Hash(p string) ([32]byte, int64, error)
}

// hashReader streams r through sha256.
func hashReader(r io.Reader) ([32]byte, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, n, err
}

// OSVolume is a mounted volume (the live environment's own kernel NFS client).
type OSVolume struct{ Root string }

func (v OSVolume) abs(p string) string { return filepath.Join(v.Root, filepath.FromSlash(p)) }

func (v OSVolume) Mkdir(p string) error { return os.MkdirAll(v.abs(p), 0o750) }

func (v OSVolume) WriteAt(p string, off int64, data []byte) error {
	f, err := os.OpenFile(v.abs(p), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteAt(data, off)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func (v OSVolume) ReadAll(p string) ([]byte, error) { return os.ReadFile(v.abs(p)) }

func (v OSVolume) Hash(p string) ([32]byte, int64, error) {
	f, err := os.Open(v.abs(p))
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer f.Close()
	return hashReader(f)
}

func (v OSVolume) Truncate(p string, size int64) error { return os.Truncate(v.abs(p), size) }

func (v OSVolume) Rename(from, to string) error { return os.Rename(v.abs(from), v.abs(to)) }

func (v OSVolume) Remove(p string) error { return os.Remove(v.abs(p)) }

func (v OSVolume) List(p string) ([]string, error) {
	ents, err := os.ReadDir(v.abs(p))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out, nil
}

func (v OSVolume) Size(p string) (int64, error) {
	st, err := os.Stat(v.abs(p))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// BillyVolume drives a billy.Filesystem (the vfs of an in-process gateway).
type BillyVolume struct{ FS billy.Filesystem }

func (v BillyVolume) Mkdir(p string) error { return v.FS.MkdirAll(p, 0o755) }

func (v BillyVolume) WriteAt(p string, off int64, data []byte) error {
	f, err := v.FS.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		_ = f.Close()
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func (v BillyVolume) ReadAll(p string) ([]byte, error) {
	f, err := v.FS.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (v BillyVolume) Hash(p string) ([32]byte, int64, error) {
	f, err := v.FS.Open(p)
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer f.Close()
	return hashReader(f)
}

func (v BillyVolume) Truncate(p string, size int64) error {
	f, err := v.FS.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	terr := f.Truncate(size)
	cerr := f.Close()
	if terr != nil {
		return terr
	}
	return cerr
}

func (v BillyVolume) Rename(from, to string) error { return v.FS.Rename(from, to) }

func (v BillyVolume) Remove(p string) error { return v.FS.Remove(p) }

func (v BillyVolume) List(p string) ([]string, error) {
	ents, err := v.FS.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out, nil
}

func (v BillyVolume) Size(p string) (int64, error) {
	st, err := v.FS.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// join builds a volume path.
func join(elem ...string) string { return path.Join(elem...) }
