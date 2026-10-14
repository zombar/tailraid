// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
	"path"
	"sort"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// File information classes a directory listing can be asked for. A client
// picks one; these three are what real clients ask for.
const (
	infoDirectoryPlain  uint8 = 1  // FileDirectoryInformation
	infoDirectoryFull   uint8 = 2  // FileFullDirectoryInformation
	infoDirectoryBoth   uint8 = 3  // FileBothDirectoryInformation
	infoDirectoryNames  uint8 = 12 // FileNamesInformation
	infoDirectoryIDBoth uint8 = 37 // FileIdBothDirectoryInformation
	infoDirectoryIDFull uint8 = 38 // FileIdFullDirectoryInformation
)

// QUERY_DIRECTORY flags.
const (
	restartScans      uint8 = 0x01
	returnSingleEntry uint8 = 0x02
	reopen            uint8 = 0x10
)

// A search is where a listing has got to. SMB reads a directory in pages: the
// first call answers what fits, and the next continues. The state has to live
// on the SERVER, keyed by the handle, because the client sends no cursor.
type search struct {
	entries []dirEntry
	next    int
}

type dirEntry struct {
	name string
	st   filesystem.Stat
}

// queryDirectory answers one page of a listing.
func (c *conn) queryDirectory(h header, body []byte, msg []byte) ([]byte, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("smb: QUERY_DIRECTORY body of %d bytes is too short", len(body))
	}
	class := body[2]
	flags := body[3]
	of := c.fileByID(body[8:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	if !of.dir {
		return errorResponse(h, statusNotADirectory), nil
	}
	defer of.share.reading()()
	patOff := int(binary.LittleEndian.Uint16(body[24:]))
	patLen := int(binary.LittleEndian.Uint16(body[26:]))
	outMax := int(binary.LittleEndian.Uint32(body[28:]))
	pattern := "*"
	if patLen > 0 && patOff >= 0 && patOff+patLen <= len(msg) {
		pattern = fromUTF16le(msg[patOff : patOff+patLen])
	}

	s := c.searches[of.id]
	if s == nil || flags&(restartScans|reopen) != 0 {
		entries, status := c.listDir(of, pattern)
		if status != statusSuccess {
			return errorResponse(h, status), nil
		}
		s = &search{entries: entries}
		c.searches[of.id] = s
	}
	if s.next >= len(s.entries) {
		// NO_MORE_FILES is how a listing ENDS. A client that gets an empty
		// success asks again, forever.
		return errorResponse(h, statusNoMoreFiles), nil
	}

	if outMax > maxTransactSize {
		outMax = maxTransactSize
	}
	out := make([]byte, 0, 4096)
	lastStart := -1
	for s.next < len(s.entries) {
		e := s.entries[s.next]
		entry := encodeDirEntry(class, e, of.ro)
		if entry == nil {
			return errorResponse(h, statusInvalidInfoClass), nil
		}
		if len(out)+len(entry) > outMax {
			break
		}
		lastStart = len(out)
		out = append(out, entry...)
		s.next++
		if flags&returnSingleEntry != 0 {
			break
		}
	}
	if lastStart < 0 {
		// Not even one entry fits what the client offered.
		return errorResponse(h, statusInvalidParameter), nil
	}
	// The last entry's "next" offset is zero: that is what ends the chain.
	binary.LittleEndian.PutUint32(out[lastStart:], 0)

	const bodyLen = 8
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+len(out))...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 9)
	binary.LittleEndian.PutUint16(rb[2:], headerLen+bodyLen)
	binary.LittleEndian.PutUint32(rb[4:], uint32(len(out)))
	copy(rb[bodyLen:], out)
	return b, nil
}

// listDir reads a directory and puts "." and ".." in front of it.
//
// Those two are not optional. A client that does not see them concludes it is
// not looking at a directory, and go-filesystems drivers do not report them.
func (c *conn) listDir(of *openFile, pattern string) ([]dirEntry, uint32) {
	self, err := of.fs().Stat(of.p())
	if err != nil {
		return nil, statusFor(err, statusObjectNameNotFound)
	}
	parent := self
	if of.p() != "/" {
		if st, err := of.fs().Stat(path.Dir(of.p())); err == nil {
			parent = st
		}
	}
	out := []dirEntry{{name: ".", st: self}, {name: "..", st: parent}}

	children, err := of.fs().ListDir(of.p())
	if err != nil {
		return nil, statusFor(err, statusObjectNameNotFound)
	}
	for _, ch := range children {
		name := ch.Name()
		if name == "." || name == ".." {
			continue // a driver that reports them itself must not report them twice
		}
		st, err := of.fs().Stat(path.Join(of.p(), name))
		if err != nil {
			continue // a name that vanished between the listing and the stat
		}
		out = append(out, dirEntry{name: name, st: st})
	}
	// A stable order, because a client paging through a listing that reorders
	// itself between pages sees entries twice or not at all.
	sort.SliceStable(out[2:], func(i, j int) bool {
		return strings.ToUpper(out[2+i].name) < strings.ToUpper(out[2+j].name)
	})

	if pattern != "" && pattern != "*" {
		kept := out[:0]
		for _, e := range out {
			if matchSMB(pattern, e.name) {
				kept = append(kept, e)
			}
		}
		out = kept
	}
	return out, statusSuccess
}

// matchSMB reports whether name is in the expression pattern, as MS-FSA 2.1.4.4 has it
// (I-237), without case because the protocol is: "*" matches any run of characters, "?"
// one, DOS_STAR ("<") any run up to and including the name's final dot, DOS_QM (">")
// one character or none at a dot or the end, DOS_DOT (a quotation mark) a dot or nothing
// at the end; every other character is itself, brackets and backslashes included.
// Windows' FindFirstFile turns "*.txt" into "<.txt" and "a?c" into "a>c". path.Match had
// read "[" as a class and the DOS wildcards as literals.
func matchSMB(pattern, name string) bool {
	if pattern == "*" || pattern == "*.*" {
		return true
	}
	p := []rune(strings.ToUpper(pattern))
	n := []rune(strings.ToUpper(name))
	if len(p) == 0 || len(n) == 0 {
		return len(p) == len(n)
	}
	lastDot := -1
	for i, r := range n {
		if r == '.' {
			lastDot = i
		}
	}
	memo := map[[2]int]bool{}
	var match func(pi, ni int) bool
	match = func(pi, ni int) bool {
		key := [2]int{pi, ni}
		if v, ok := memo[key]; ok {
			return v
		}
		ok := false
		switch {
		case pi == len(p):
			ok = ni == len(n)
		case p[pi] == '*':
			for k := ni; k <= len(n) && !ok; k++ {
				ok = match(pi+1, k)
			}
		case p[pi] == '<': // up to and including the final dot, never past it
			end := len(n)
			if lastDot >= ni {
				end = lastDot + 1
			}
			for k := ni; k <= end && !ok; k++ {
				ok = match(pi+1, k)
			}
		case p[pi] == '>':
			if ni == len(n) || n[ni] == '.' {
				ok = match(pi+1, ni) // none at a dot or the end
			} else {
				ok = match(pi+1, ni+1)
			}
		case p[pi] == '"':
			switch {
			case ni == len(n):
				ok = match(pi+1, ni)
			case n[ni] == '.':
				ok = match(pi+1, ni+1)
			}
		case ni == len(n):
		case p[pi] == '?' || p[pi] == n[ni]:
			ok = match(pi+1, ni+1)
		}
		memo[key] = ok
		return ok
	}
	return match(0, 0)
}

// encodeDirEntry lays out one entry in the class the client asked for. A class
// this server does not write returns nil, and the caller refuses by name.
func encodeDirEntry(class uint8, e dirEntry, readOnly bool) []byte {
	name := utf16le(e.name)
	// Every class is the same fields in the same order, differing only in what
	// is bolted on after the name length -- which is why the layout below is
	// one path with the name at a different offset.
	//
	// Which class matters: the Linux kernel's client asks for
	// FileIdFullDirectoryInformation and NOTHING else, so a server that writes
	// only the two macOS uses answers INVALID_INFO_CLASS and the mount reports
	// EIO on its first readdir, with nothing in the kernel log.
	var fixed int
	switch class {
	case infoDirectoryIDBoth:
		fixed = 104
	case infoDirectoryBoth:
		fixed = 94
	case infoDirectoryIDFull:
		fixed = 80
	case infoDirectoryFull:
		fixed = 68
	case infoDirectoryPlain:
		fixed = 64
	case infoDirectoryNames:
		fixed = 12
	default:
		return nil
	}
	// Every entry starts on an eight-byte boundary, and the padding is part of
	// the entry rather than between entries: the next offset points at the
	// next entry, so it has to count it.
	size := (fixed + len(name) + 7) / 8 * 8
	b := make([]byte, size)
	binary.LittleEndian.PutUint32(b[0:], uint32(size))

	if class == infoDirectoryNames {
		binary.LittleEndian.PutUint32(b[8:], uint32(len(name)))
		copy(b[12:], name)
		return b
	}

	writeTimes(b[8:], e.st)
	sz := sizeOf(e.st)
	binary.LittleEndian.PutUint64(b[40:], sz)
	binary.LittleEndian.PutUint64(b[48:], allocationOf(sz))
	binary.LittleEndian.PutUint32(b[56:], attributesOf(e.name, e.st, readOnly))
	binary.LittleEndian.PutUint32(b[60:], uint32(len(name)))
	switch class {
	case infoDirectoryIDFull:
		binary.LittleEndian.PutUint64(b[72:], e.st.Inode())
	case infoDirectoryIDBoth:
		binary.LittleEndian.PutUint64(b[96:], e.st.Inode())
	}
	// ShortNameLength, where the class has one, stays zero: there are no 8.3
	// names here and a client asked to display one would show blanks.
	copy(b[fixed:], name)
	return b
}
