//go:build !plan9

package smb

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// refusingFS is tinyFS whose writes fail with one chosen error, the way a
// directory share's do when the volume under it is full.
type refusingFS struct {
	tinyFS
	err error
}

func (f *refusingFS) WriteFile(string, []byte, os.FileMode) error { return f.err }

// TestAFullShareAnswersDiskFull: a WRITE the filesystem refuses for lack of
// space, or of quota, must not read as ACCESS_DENIED -- a client shows that
// as "you do not have permission", and the user goes looking at ACLs while
// the disk is full.
//
// The expected values are written out from MS-ERREF §2.3.1, not taken from
// this package's constants:
//
//	0xC000007F STATUS_DISK_FULL  "An operation failed because the disk was full."
//	0xC0000022 STATUS_ACCESS_DENIED
//
// Which errno gets which status follows Samba, the server Windows clients are
// tested against: source3/lib/errmap_unix.c, unix_nt_errmap as read by
// map_nt_error_from_unix, maps ENOSPC, EDQUOT and EFBIG all to
// NT_STATUS_DISK_FULL -- EDQUOT with the note "Windows apps need this, not
// NT_STATUS_QUOTA_EXCEEDED" (0xC0000044).
func TestAFullShareAnswersDiskFull(t *testing.T) {
	const (
		statusDiskFullERREF     = 0xC000007F // MS-ERREF §2.3.1 STATUS_DISK_FULL
		statusAccessDeniedERREF = 0xC0000022 // MS-ERREF §2.3.1 STATUS_ACCESS_DENIED
	)
	for _, tc := range []struct {
		name string
		err  error
		want uint32
	}{
		{"ENOSPC", &os.PathError{Op: "write", Path: "/file.txt", Err: syscall.ENOSPC}, statusDiskFullERREF},
		{"EDQUOT", &os.PathError{Op: "write", Path: "/file.txt", Err: syscall.EDQUOT}, statusDiskFullERREF},
		{"EFBIG", &os.PathError{Op: "write", Path: "/file.txt", Err: syscall.EFBIG}, statusDiskFullERREF},
		{"ENOSPC wrapped in other words", fmt.Errorf("store refused the block: %w", syscall.ENOSPC), statusDiskFullERREF},
		// The control: a real permission refusal still says so.
		{"EACCES", &os.PathError{Op: "write", Path: "/file.txt", Err: syscall.EACCES}, statusAccessDeniedERREF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, of := opened(t, &refusingFS{tinyFS: tinyFS{body: []byte("hello")}, err: tc.err}, "/file.txt", false)
			body := make([]byte, 48+4)
			binary.LittleEndian.PutUint16(body[2:], uint16(headerLen+48))
			binary.LittleEndian.PutUint32(body[4:], 4)
			copy(body[16:], of.id[:])
			copy(body[48:], "data")
			if got := statusOf(t, mustDispatch(t, c, cmdWrite, body)); got != tc.want {
				t.Fatalf("WRITE refused with %v answered %#08x, want %#08x", tc.err, got, uint32(tc.want))
			}
		})
	}
}
