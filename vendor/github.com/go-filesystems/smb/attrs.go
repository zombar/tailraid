// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	filesystem "github.com/go-filesystems/interface"
)

// The POSIX type bits a driver reports in Stat.Mode.
const (
	sIFMT  = 0xF000
	sIFDIR = 0x4000
	sIFLNK = 0xA000
	sIFREG = 0x8000
)

// Windows file attributes. A client reads these far more closely than the
// mode: DIRECTORY is what makes it try to enumerate, and READONLY is what
// makes it grey out a rename.
const (
	attrReadOnly  uint32 = 0x00000001
	attrHidden    uint32 = 0x00000002
	attrDirectory uint32 = 0x00000010
	attrArchive   uint32 = 0x00000020
	attrNormal    uint32 = 0x00000080
)

func isDir(st filesystem.Stat) bool { return st.Mode()&sIFMT == sIFDIR }

// attributesOf maps a driver's mode onto the attributes a client reads.
//
// A mode with no type bits at all -- a driver that reports permissions only --
// is a regular file: it is the only guess that cannot make a client try to
// enumerate something that is not a directory.
func attributesOf(st filesystem.Stat, readOnly bool) uint32 {
	var a uint32
	if isDir(st) {
		a |= attrDirectory
	} else {
		a |= attrArchive
	}
	// No write bit for anyone, or a share exported read-only: the client is
	// told before it tries.
	if readOnly || st.Mode()&0o222 == 0 {
		a |= attrReadOnly
	}
	if a == 0 {
		a = attrNormal
	}
	return a
}

// sizeOf is the size a client shows. A directory's own size is meaningless to
// SMB and every server reports zero for it.
func sizeOf(st filesystem.Stat) uint64 {
	if isDir(st) {
		return 0
	}
	return st.Size()
}

// allocationOf rounds a size up to a cluster, which is what a client shows as
// "size on disk". Rounding to 4 KiB is a statement about the CLIENT's display,
// not about the driver: go-filesystems does not report an allocated size, and
// reporting the exact byte count would make every file appear to occupy no
// slack at all.
func allocationOf(size uint64) uint64 {
	const cluster = 4096
	return (size + cluster - 1) / cluster * cluster
}
