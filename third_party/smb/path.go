// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"path"
	"strings"
)

// SMB paths arrive with backslashes, relative to the share root, and with no
// leading separator: "sub\dir\file.txt", and "" for the root itself. A
// go-filesystems driver wants "/sub/dir/file.txt".
//
// The translation is also where a path that tries to leave the share is
// refused. path.Clean resolves "..", so a name that climbs out lands outside
// the root and is caught by comparing the result -- rather than by looking for
// the string "..", which a client can spell in ways a scan misses.
func smbPathToFS(p string) (string, bool) {
	p = strings.ReplaceAll(p, `\`, "/")
	// A trailing stream name is not a path: "file.txt:stream:$DATA" names an
	// alternate data stream, which this server does not have.
	if strings.Contains(p, ":") {
		return "", false
	}
	clean := path.Clean("/" + p)
	if clean != "/" && (strings.HasPrefix(clean, "../") || clean == "..") {
		return "", false
	}
	return clean, true
}

// fsPathToSMB is the way back, for a name a client will show.
func fsPathToSMB(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(p, "/", `\`), `\`)
}

// baseName is the last element, which is what a directory listing prints.
func baseName(p string) string {
	if p == "/" {
		return ""
	}
	return path.Base(p)
}
