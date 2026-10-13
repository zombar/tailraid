package smb

import (
	"encoding/binary"
	"testing"
)

const (
	aRead   uint32 = 0x00000001 // FILE_READ_DATA
	aWrite  uint32 = 0x00000002 // FILE_WRITE_DATA
	aAttrs  uint32 = 0x00000080 // FILE_READ_ATTRIBUTES
	aDelete uint32 = 0x00010000 // DELETE
	sRead   uint32 = 0x1
	sWrite  uint32 = 0x2
	sDelete uint32 = 0x4
)

// openWith sends a CREATE of /file.txt with the access, share access and disposition
// given, and returns its status and, on success, the handle.
func openWith(t *testing.T, c *conn, access, share, disposition uint32) (uint32, [16]byte) {
	t.Helper()
	name := utf16le("file.txt")
	body := make([]byte, 56)
	binary.LittleEndian.PutUint32(body[24:], access)
	binary.LittleEndian.PutUint32(body[32:], share)
	binary.LittleEndian.PutUint32(body[36:], disposition)
	binary.LittleEndian.PutUint16(body[44:], uint16(headerLen+56))
	binary.LittleEndian.PutUint16(body[46:], uint16(len(name)))
	out := mustDispatch(t, c, cmdCreate, append(body, name...))
	st := statusOf(t, out)
	var id [16]byte
	if st == statusSuccess {
		copy(id[:], out[headerLen+64:])
	}
	return st, id
}

func closeID(t *testing.T, c *conn, id [16]byte) {
	t.Helper()
	b := make([]byte, 24)
	copy(b[8:], id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdClose, b)); st != statusSuccess {
		t.Fatalf("close answered %#x", st)
	}
}

// An open that asks for data access another open of the file does not share, or that
// does not share the data access another open holds, is refused with
// STATUS_SHARING_VIOLATION before it changes anything (MS-FSA 2.1.5.1.2.1); an open for
// attributes only never conflicts; a closed handle no longer counts. Before, share
// access was not read at all: two programs that each opened a file without sharing both
// got it, and an overwrite truncated a file another held exclusively.
// Invariant: I-265
func TestCreate_ShareAccess(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, _ := opened(t, fs, "/file.txt", false)

	stA, a := openWith(t, c, aRead|aWrite, sRead, dispOpen)
	if stA != statusSuccess {
		t.Fatalf("the first open answered %#x", stA)
	}
	for _, tc := range []struct {
		name          string
		access, share uint32
		want          uint32
	}{
		{"write, which A does not share", aWrite, sRead | sWrite | sDelete, statusSharingViolation},
		{"read, not sharing the write A holds", aRead, sRead, statusSharingViolation},
		{"read, sharing read and write", aRead, sRead | sWrite, statusSuccess},
		{"delete, which A does not share", aDelete, sRead | sWrite | sDelete, statusSharingViolation},
		{"attributes only, sharing nothing", aAttrs, 0, statusSuccess},
	} {
		st, id := openWith(t, c, tc.access, tc.share, dispOpen)
		if st != tc.want {
			t.Errorf("%s: answered %#x, want %#x", tc.name, st, tc.want)
		}
		if st == statusSuccess {
			closeID(t, c, id)
		}
	}
	// an overwrite that would violate A's sharing changes nothing
	if st, _ := openWith(t, c, aWrite, sRead|sWrite|sDelete, dispOverwrite); st != statusSharingViolation {
		t.Fatalf("an overwrite against an exclusive writer answered %#x", st)
	}
	if string(fs.body) != "hello" {
		t.Fatalf("a refused overwrite truncated the file to %q", fs.body)
	}
	closeID(t, c, a)
	if st, id := openWith(t, c, aWrite, sRead, dispOpen); st != statusSuccess {
		t.Fatalf("after A closed, a writer answered %#x", st)
	} else {
		closeID(t, c, id)
	}
}
