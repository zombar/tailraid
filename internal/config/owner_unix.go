//go:build unix

package config

import (
	"io/fs"
	"syscall"
)

// ownedBy is the uid that owns the file st describes, and whether the OS said.
func ownedBy(st fs.FileInfo) (uid int, ok bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(s.Uid), true
}
