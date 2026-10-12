package smb2

import "github.com/cloudsoda/go-smb2/internal/smb2"

// tailraid: the two calls its soak driver needs to act as Windows Explorer does and
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
