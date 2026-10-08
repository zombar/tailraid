//go:build windows

package vfs

// Windows cannot fsync a directory handle; the rename itself is journaled by NTFS.
func isDirSyncUnsupported(error) bool { return true }
