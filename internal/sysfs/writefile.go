package sysfs

import (
	"os"
	"path/filepath"
)

// WriteFileSync writes data to path atomically: a temp file beside it, fsynced, chmod
// 0600, renamed over the target, the directory synced (the identity files' rule, I-96;
// the stats spill of docs/17 §2).
func WriteFileSync(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return OS{}.SyncDir(dir) // a directory cannot be opened for fsync on Windows; the seam knows
}
