//go:build !windows

package vfs

func isDirSyncUnsupported(error) bool { return false }
