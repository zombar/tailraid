package chunk

import (
	"path/filepath"

	"github.com/zombar/tailraid/internal/sysfs"
)

// privateDir creates dir 0700, or tightens one that exists and is wider: a tree a release
// before I-340 created was 0755, and another user of the device could list shard names,
// never read them (docs/08 §Files on disk).
func privateDir(fsys sysfs.FS, dir string) error {
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := fsys.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	return fsys.Chmod(dir, 0o700)
}

// privateTree is privateDir on dir and on every directory one level under it: the
// store's and the cache's prefix directories.
func privateTree(fsys sysfs.FS, dir string) error {
	if err := privateDir(fsys, dir); err != nil {
		return err
	}
	ents, err := fsys.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if err := privateDir(fsys, filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
