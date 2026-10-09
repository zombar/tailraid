//go:build !windows

package sysfs

import "os"

func syncDir(name string) error {
	d, err := os.Open(name) //nolint:gosec // the FS seam opens the directory the store names
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
