//go:build !windows

package sysfs

import "syscall"

// diskFree is statfs: bytes available to an unprivileged caller and the filesystem size.
func diskFree(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize) //nolint:unconvert // Bsize is int64 on linux and uint32 on darwin
	return st.Bavail * bsize, st.Blocks * bsize, nil
}
