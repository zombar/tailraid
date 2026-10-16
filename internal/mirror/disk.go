package mirror

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/zombar/tailraid/internal/sysfs"
)

// writeDurable writes b to final through a temp in tmpDir: write, fsync, close, rename,
// fsync of final's directory, the shard store's sequence (docs/01, I-180). On any error
// the temp is removed and final is untouched.
//
// The mirror's root must exist: a disk unplugged mid-sync leaves its mount point behind,
// and recreating the path would write the mirror onto the host's own disk, under the
// mount point, where the disk hides it once it is back.
func writeDurable(fsys sysfs.FS, tmpDir, final string, b []byte) error {
	if err := mkdirBelow(fsys, filepath.Dir(tmpDir), tmpDir); err != nil {
		return err
	}
	f, err := fsys.CreateTemp(tmpDir, "w-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error { _ = f.Close(); _ = fsys.Remove(tmp); return err }
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = fsys.Remove(tmp)
		return err
	}
	if err := fsys.Rename(tmp, final); err != nil {
		_ = fsys.Remove(tmp)
		return err
	}
	return fsys.SyncDir(filepath.Dir(final))
}

func readAll(fsys sysfs.FS, p string) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// walkTwoLevel calls fn for every file under root/<hh>/ with its name and size.
func walkTwoLevel(fsys sysfs.FS, root string, fn func(name string, size int64)) error {
	subs, err := fsys.ReadDir(root)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !sub.IsDir() {
			continue
		}
		ents, err := fsys.ReadDir(filepath.Join(root, sub.Name()))
		if err != nil {
			return err
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			var size int64
			if fi, err := e.Info(); err == nil {
				size = fi.Size()
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			fn(e.Name(), size)
		}
	}
	return nil
}

// mkdirBelow makes dir and the directories between it and root one level at a time,
// never root itself: an unplugged root fails the first make, wherever the unplug lands,
// where a look at the root followed by MkdirAll would recreate it (I-180).
func mkdirBelow(fsys sysfs.FS, root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("mirror: %s is not below %s", dir, root)
	}
	if rel == "." {
		_, err := fsys.Stat(root)
		return err
	}
	p := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		if err := fsys.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}
