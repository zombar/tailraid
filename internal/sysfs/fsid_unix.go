//go:build !windows

package sysfs

import (
	"strconv"
	"syscall"
)

// FilesystemID names the filesystem holding dir (its device number), so callers can tell
// two directories on one disk apart from two disks (docs/14 §3).
func FilesystemID(dir string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(st.Dev), 10), nil //nolint:unconvert,gosec // Dev is int32 on darwin, uint64 on linux
}
