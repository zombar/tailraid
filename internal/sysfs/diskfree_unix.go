//go:build !windows && !linux

package sysfs

import "syscall"

// diskFree is statfs: bytes available to an unprivileged caller and the filesystem size.
// Linux has its own file: there the unit of Bavail and Blocks is Frsize.
func diskFree(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return st.Bavail * bsize, st.Blocks * bsize, nil
}
