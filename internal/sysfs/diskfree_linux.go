package sysfs

import "syscall"

// diskFree is statfs: bytes available to an unprivileged caller and the filesystem size.
// On Linux the unit of Bavail and Blocks is Frsize, the fragment size, which differs from
// Bsize on some filesystems (XFS with large block sizes, some FUSE mounts).
func diskFree(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	unit := uint64(st.Frsize)
	if unit == 0 {
		unit = uint64(st.Bsize)
	}
	return st.Bavail * unit, st.Blocks * unit, nil
}
