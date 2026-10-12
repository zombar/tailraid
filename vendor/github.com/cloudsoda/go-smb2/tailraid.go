package smb2

import (
	"encoding/binary"

	"github.com/cloudsoda/go-smb2/internal/smb2"
)

// tailraid: the calls its soak driver needs to act as Windows Explorer does and
// that upstream does not export (docs/09 §6, phase 23). Everything else in this
// directory is cloudsoda/go-smb2 as vendored at v0.0.0-20260918041005-0c5d69b69701,
// without its tests and its ntstatus generator (internal/erref/mkntstatus.go).

// SetDeleteOnClose marks the open file delete-on-close (SET_INFO
// FileDispositionInformation), as a copy engine does to remove a file it failed to
// copy: the server deletes it when the handle closes.
func (f *File) SetDeleteOnClose() error { return f.remove() }

// RenameTo renames the open file through its own handle. Without replace an existing
// newpath is the server's to refuse (STATUS_OBJECT_NAME_COLLISION), as a rename in
// Explorer asks; upstream's Share.Rename always replaces.
func (f *File) RenameTo(newpath string, replace bool) error {
	newpath = normPath(newpath)
	if err := validatePath("rename to", newpath, false); err != nil {
		return err
	}
	var r uint8
	if replace {
		r = 1
	}
	return f.setInfo(&smb2.SetInfoRequest{
		FileInfoClass: smb2.FileRenameInformation,
		Input: &smb2.FileRenameInformationType2Encoder{
			ReplaceIfExists: r,
			FileName:        newpath,
			Mapping:         f.mapping,
		},
	})
}

// QueryFileRegions sends FSCTL_QUERY_FILE_REGIONS (MS-FSCC 2.3.55) with a
// FILE_REGION_INPUT of off, length and usage, as Windows' copy engine asks of a
// destination it has just created and sized, and returns the FILE_REGION_OUTPUT bytes.
// A refusal is the error, with whatever output came with it.
func (f *File) QueryFileRegions(off, length int64, usage uint32, maxOut uint32) ([]byte, error) {
	return f.ioctl(&smb2.IoctlRequest{
		CtlCode:           0x00090284,
		MaxOutputResponse: maxOut,
		Flags:             smb2.SMB2_0_IOCTL_IS_FSCTL,
		Input:             regionInput{off, length, usage},
	})
}

type regionInput struct {
	off, length int64
	usage       uint32
}

func (r regionInput) Size() int { return 24 }

func (r regionInput) Encode(b []byte) {
	binary.LittleEndian.PutUint64(b[0:], uint64(r.off))    //nolint:gosec // the wire field is a signed 64-bit integer
	binary.LittleEndian.PutUint64(b[8:], uint64(r.length)) //nolint:gosec // as above
	binary.LittleEndian.PutUint32(b[16:], r.usage)
	binary.LittleEndian.PutUint32(b[20:], 0)
}
