package smb

import (
	"encoding/binary"
	"os"
	"testing"
)

// checkingFS refuses to delete its one file.
type checkingFS struct{ *tinyFS }

func (checkingFS) CanDelete(p string) error {
	if p == "/file.txt" {
		return os.ErrPermission
	}
	return nil
}

// A backend that can refuse a delete is asked when the client marks the handle
// delete-on-close, and the client reads its status: the delete itself happens at
// CLOSE, whose reply carries none.
func TestDeleteOnClose_AsksTheBackend(t *testing.T) {
	fs := checkingFS{&tinyFS{body: []byte("hello")}}
	c, of := opened(t, fs, "/file.txt", false)
	set := func(in []byte) uint32 {
		body := make([]byte, 32)
		body[2], body[3] = infoTypeFile, fileDispositionInformation
		binary.LittleEndian.PutUint32(body[4:], uint32(len(in)))
		binary.LittleEndian.PutUint16(body[8:], uint16(headerLen+32))
		copy(body[16:], of.id[:])
		out, err := c.dispatch(request(cmdSetInfo, 1, append(body, in...)))
		if err != nil {
			t.Fatal(err)
		}
		return statusOf(t, out)
	}
	if st := set([]byte{1}); st != statusAccessDenied {
		t.Errorf("marking a refused delete answered %#x, want STATUS_ACCESS_DENIED", st)
	}
	if of.deleteOnClose {
		t.Error("the handle was marked delete-on-close after the backend refused")
	}
	// clearing the mark asks nothing
	if st := set([]byte{0}); st != statusSuccess {
		t.Errorf("clearing delete-on-close answered %#x", st)
	}
	// a backend without the capability is never asked (the plain tinyFS marks as before)
	plain, pof := opened(t, &tinyFS{body: []byte("hello")}, "/file.txt", false)
	body := make([]byte, 32)
	body[2], body[3] = infoTypeFile, fileDispositionInformation
	binary.LittleEndian.PutUint32(body[4:], 1)
	binary.LittleEndian.PutUint16(body[8:], uint16(headerLen+32))
	copy(body[16:], pof.id[:])
	out, err := plain.dispatch(request(cmdSetInfo, 1, append(body, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusSuccess || !pof.deleteOnClose {
		t.Errorf("a backend without the capability: status %#x, marked %v", st, pof.deleteOnClose)
	}
}
