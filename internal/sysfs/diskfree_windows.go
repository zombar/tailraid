//go:build windows

package sysfs

import "golang.org/x/sys/windows"

// diskFree is GetDiskFreeSpaceEx: bytes available to the caller and the volume size.
func diskFree(dir string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var avail, size, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &size, &totalFree); err != nil {
		return 0, 0, err
	}
	return avail, size, nil
}
