package nfs

// tailraid: a listing reply is filled by its entries' real XDR sizes (docs/07, I-248).
// Upstream charged every entry 512 bytes, so a READDIR of short names returned about a
// sixteenth of what the client's count had room for and a READDIRPLUS about a third.

// fattr3Size is an fattr3 on the wire: five 32-bit fields and eight 64-bit ones.
const fattr3Size = 5*4 + 8*8

// listOverhead is a READDIR(PLUS)3resok without entries: the status, the directory's
// post_op_attr, the cookie verifier, the list terminator and eof.
const listOverhead = 4 + 4 + fattr3Size + 8 + 4 + 4

// xdrPad is n rounded up to XDR's four-byte unit.
func xdrPad(n int) uint32 { return uint32((n + 3) &^ 3) }

// dirInfoSize is what an entry costs against READDIRPLUS's dircount (RFC 1813: the
// directory information, without attributes and handle): fileid, name, cookie.
func dirInfoSize(name string) uint32 { return 8 + 4 + xdrPad(len(name)) + 8 }

// entry3Size is a READDIR entry3 on the wire, with the value-follows flag before it.
func entry3Size(name string) uint32 { return 4 + dirInfoSize(name) }

// entryPlus3Size is a READDIRPLUS entryplus3 on the wire: entry3 plus the post_op_attr
// and the post_op_fh3 (handleLen < 0: no handle).
func entryPlus3Size(name string, attrs bool, handleLen int) uint32 {
	n := entry3Size(name) + 4
	if attrs {
		n += fattr3Size
	}
	n += 4
	if handleLen >= 0 {
		n += 4 + xdrPad(handleLen)
	}
	return n
}
