//go:build windows

package sysfs

// Windows has no directory fsync; NTFS metadata updates for a rename are journaled.
func syncDir(string) error { return nil }
