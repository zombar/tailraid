// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"path"
	"sort"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// SMB is caseless, and this server says so: the FileFsAttributeInformation it
// answers carries CASE_PRESERVED_NAMES and not CASE_SENSITIVE_SEARCH. A client
// takes that at its word -- Windows especially, which will happily ask for
// PROGRAM.EXE having been shown program.exe.
//
// The drivers underneath do not agree with each other. FAT32, exFAT and NTFS
// compare without case themselves, so nothing here is needed for them. ext4
// and the rest are case-sensitive, and on those the promise was a lie: a
// client that asked for the name it had just been shown, in another case, was
// told the file does not exist.
//
// So: try the name as it came, and only if that finds nothing, look for one
// that differs by case alone. The cost falls entirely on the miss, which means
// nothing at all on a driver that was already caseless.
func resolveCase(fsys filesystem.Filesystem, p string) string {
	if _, err := fsys.Stat(p); err == nil {
		return p
	}
	if p == "/" {
		return p
	}
	// Walk down from the root, folding one element at a time. A parent whose
	// case is wrong has to be found before its child can be.
	resolved := "/"
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, want := range parts {
		if want == "" {
			continue
		}
		next := path.Join(resolved, want)
		if _, err := fsys.Stat(next); err == nil {
			resolved = next
			continue
		}
		match, found := foldedMatch(fsys, resolved, want)
		if !found {
			// Nothing here differs by case either: the rest is new, and goes under the
			// folders that do exist as they are spelled there. Returning the client's
			// spelling whole made a create of photos\new.jpg, with Photos there, a
			// second folder photos (I-233).
			return path.Join(append([]string{resolved}, parts[i:]...)...)
		}
		resolved = path.Join(resolved, match)
	}
	return resolved
}

// parentMissing reports whether the folder p would live in does not exist (or is not a
// folder): MS-FSA fails such an open with STATUS_OBJECT_PATH_NOT_FOUND, and a backend that
// would make the folders on the way must not be asked to (I-233).
func parentMissing(fsys filesystem.Filesystem, p string) bool {
	st, err := fsys.Stat(path.Dir(p))
	return err != nil || !isDir(st)
}

// renameTarget is where a rename of the handle's file ino to to lands: to's folder as it
// exists, and a leaf that names another file only by case taken as that file, so replace
// and no-replace act on it rather than making a twin that differs by case. A leaf that
// differs only by case from the file itself is a case-only rename and is kept as asked.
func renameTarget(fsys filesystem.Filesystem, to string, ino uint64) (string, bool) {
	dir := resolveCase(fsys, path.Dir(to))
	if parentMissing(fsys, path.Join(dir, "x")) {
		return "", false
	}
	target := path.Join(dir, path.Base(to))
	if _, err := fsys.Stat(target); err == nil {
		return target, true
	}
	if m, found := foldedMatch(fsys, dir, path.Base(to)); found {
		other := path.Join(dir, m)
		if st, err := fsys.Stat(other); err == nil && (ino == 0 || st.Inode() != ino) {
			return other, true
		}
	}
	return target, true
}

// foldedMatch finds the one entry of dir whose name differs from want only by
// case.
//
// Two entries can differ by case alone on a case-sensitive driver -- README
// and readme in one directory is legal on ext4 -- and there is no right answer
// then. The names are sorted and the first is taken, so at least the same
// client asking twice gets the same file.
func foldedMatch(fsys filesystem.Filesystem, dir, want string) (string, bool) {
	entries, err := fsys.ListDir(dir)
	if err != nil {
		return "", false
	}
	var matches []string
	for _, e := range entries {
		if strings.EqualFold(e.Name(), want) {
			matches = append(matches, e.Name())
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	return matches[0], true
}
