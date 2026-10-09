//go:build windows

package sysfs

import (
	"path/filepath"
	"strings"
)

// FilesystemID names the volume holding dir (its volume name), so callers can tell two
// directories on one disk apart from two disks (docs/14 §3).
func FilesystemID(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(filepath.VolumeName(abs)), nil
}
