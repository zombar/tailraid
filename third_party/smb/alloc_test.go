// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"testing"
)

// setInfoReq is a SET_INFO request body for the handle id: file information class
// class with the given input.
func setInfoReq(class uint8, id [16]byte, in []byte) []byte {
	body := make([]byte, 32, 32+len(in))
	binary.LittleEndian.PutUint16(body[0:], 33)
	body[2], body[3] = infoTypeFile, class
	binary.LittleEndian.PutUint32(body[4:], uint32(len(in)))
	binary.LittleEndian.PutUint16(body[8:], uint16(headerLen+32))
	copy(body[16:], id[:])
	return append(body, in...)
}

// FileAllocationInformation reserves space; it is not the end of file (MS-FSA
// 2.1.5.15.1): a size above the file's leaves the file as it is, a size below it cuts
// the file there. It had been an end-of-file, so a program that preallocated and wrote
// less left a file padded with zeros.
// Invariant: I-235
func TestSetInfo_Allocation_NotEndOfFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alloc uint64
		want  string
	}{
		{"above the size", 1 << 20, "hello"},
		{"at the size", 5, "hello"},
		{"below the size", 3, "hel"},
		{"zero", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, of := opened(t, &tinyFS{body: []byte("hello")}, "/file.txt", false)
			w := &wfile{data: []byte("hello")}
			of.f, of.w = w, w
			in := make([]byte, 8)
			binary.LittleEndian.PutUint64(in, tc.alloc)
			if st := statusOf(t, mustDispatch(t, c, cmdSetInfo, setInfoReq(fileAllocationInformation, of.id, in))); st != statusSuccess {
				t.Fatalf("status %#x", st)
			}
			if string(w.data) != tc.want {
				t.Fatalf("file of %d bytes after an allocation of %d, want %q", len(w.data), tc.alloc, tc.want)
			}
		})
	}
}
