// Package soak runs the scenario sequence of docs/09 §6 against an Env (compose, live or
// the in-process harness) and renders the report.
package soak

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

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

// OSVolume is a mounted volume (the live environment's own kernel NFS or SMB client).
// A Windows SMB mapping comes back after its gateway restarts only on a later access,
// and the redirector reports "network path not found" for the few seconds it remembers
// the failure, so on Windows an operation failing that way is retried for a bounded
// time (Transient, docs/15 §8); the NFS clients ride out a restart on their own.
type OSVolume struct {
	Root string
	// Transient reports an error worth retrying for RetryFor; nil retries nothing.
	Transient func(error) bool
	RetryFor  time.Duration
}

func (v OSVolume) abs(p string) string { return filepath.Join(v.Root, filepath.FromSlash(p)) }

// retry runs op until it returns nil, a non-transient error, or RetryFor has passed.
func (v OSVolume) retry(op func() error) error {
	deadline := time.Now().Add(v.RetryFor)
	for {
		err := op()
		if err == nil || v.Transient == nil || !v.Transient(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second) //nolint:forbidigo // the OS client's reconnection is wall-clock time
	}
}

func (v OSVolume) Mkdir(p string) error {
	return v.retry(func() error { return os.MkdirAll(v.abs(p), 0o750) })
}

func (v OSVolume) WriteAt(p string, off int64, data []byte) error {
	return v.retry(func() error {
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
	})
}

func (v OSVolume) ReadAll(p string) (data []byte, err error) {
	err = v.retry(func() error { data, err = os.ReadFile(v.abs(p)); return err })
	return data, err
}

func (v OSVolume) Hash(p string) (sum [32]byte, n int64, err error) {
	err = v.retry(func() error {
		f, err := os.Open(v.abs(p))
		if err != nil {
			return err
		}
		defer f.Close()
		sum, n, err = hashReader(f)
		return err
	})
	return sum, n, err
}

func (v OSVolume) Truncate(p string, size int64) error {
	return v.retry(func() error { return os.Truncate(v.abs(p), size) })
}

func (v OSVolume) Rename(from, to string) error {
	return v.retry(func() error { return os.Rename(v.abs(from), v.abs(to)) })
}

func (v OSVolume) Remove(p string) error { return v.retry(func() error { return os.Remove(v.abs(p)) }) }

func (v OSVolume) List(p string) (out []string, err error) {
	err = v.retry(func() error {
		ents, err := os.ReadDir(v.abs(p))
		if err != nil {
			return err
		}
		out = make([]string, 0, len(ents))
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return nil
	})
	return out, err
}

func (v OSVolume) Size(p string) (size int64, err error) {
	err = v.retry(func() error {
		st, err := os.Stat(v.abs(p))
		if err != nil {
			return err
		}
		size = st.Size()
		return nil
	})
	return size, err
}

// TransientNetError reports the errors a Windows SMB mapping gives while its gateway is
// back but the redirector has not reconnected: ERROR_BAD_NETPATH (53), ERROR_BAD_NET_RESP
// (58), ERROR_UNEXP_NET_ERR (59) and ERROR_NETNAME_DELETED (64). Nothing on the other
// OSes: their NFS clients ride out a gateway restart.
func TransientNetError(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == 53 || errno == 58 || errno == 59 || errno == 64
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

// AttrVolume is a Volume that sets and reads a file's mode and times, as a copy does
// (soak phase 34); a volume without it skips the phase.
type AttrVolume interface {
	SetAttr(p string, mode uint32, mtime time.Time) error
	Attrs(p string) (mode uint32, mtime time.Time, err error)
}

// SetAttr is chmod and a utime on the mount.
func (v OSVolume) SetAttr(p string, mode uint32, mtime time.Time) error {
	if err := os.Chmod(v.abs(p), os.FileMode(mode)); err != nil {
		return err
	}
	return os.Chtimes(v.abs(p), mtime, mtime)
}

// Attrs is a stat on the mount.
func (v OSVolume) Attrs(p string) (uint32, time.Time, error) {
	fi, err := os.Stat(v.abs(p))
	if err != nil {
		return 0, time.Time{}, err
	}
	return uint32(fi.Mode().Perm()), fi.ModTime(), nil
}

// SetAttr is the filesystem's Chmod and Chtimes.
func (v BillyVolume) SetAttr(p string, mode uint32, mtime time.Time) error {
	ch, ok := v.FS.(billy.Change)
	if !ok {
		return errors.New("soak: the filesystem sets no attributes")
	}
	if err := ch.Chmod(p, os.FileMode(mode)); err != nil {
		return err
	}
	return ch.Chtimes(p, mtime, mtime)
}

// Attrs is the filesystem's Stat.
func (v BillyVolume) Attrs(p string) (uint32, time.Time, error) {
	fi, err := v.FS.Stat(p)
	if err != nil {
		return 0, time.Time{}, err
	}
	return uint32(fi.Mode().Perm()), fi.ModTime(), nil
}
