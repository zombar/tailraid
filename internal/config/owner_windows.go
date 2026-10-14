//go:build windows

package config

import "io/fs"

// ownedBy: Windows has no uid; Warnings returns before asking.
func ownedBy(fs.FileInfo) (uid int, ok bool) { return 0, false }
