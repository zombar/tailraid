//go:build !windows

package wintun

// EnsureDLL has nothing to write off Windows.
func EnsureDLL(string) (written bool, err error) { return false, nil }
