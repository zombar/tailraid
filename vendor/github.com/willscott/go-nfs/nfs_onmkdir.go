package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/xdr"
)

const (
	mkdirDefaultMode = 755
)

func onMkdir(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter
	obj := DirOpArg{}
	err := xdr.Read(w.req.Body, &obj)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	attrs, err := ReadSetFileAttributes(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	fs, path, err := userHandle.FromHandle(obj.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	w.errorFmt = dirWccErrorFormatter(fs, path) // tailraid: a refusal carries the parent's attributes (I-242)
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}

	if len(string(obj.Filename)) > PathNameMax {
		return &NFSStatusError{NFSStatusNameTooLong, os.ErrInvalid}
	}
	if string(obj.Filename) == "." || string(obj.Filename) == ".." {
		return &NFSStatusError{NFSStatusExist, os.ErrExist}
	}

	newFolder := append(path, string(obj.Filename))
	newFolderPath := fs.Join(newFolder...)
	if _, err := fs.Stat(newFolderPath); err == nil {
		return &NFSStatusError{NFSStatusExist, nil} // a folder or a file: RFC 1813 §3.3.9 (tailraid, I-238)
	} else {
		if s, err := fs.Stat(fs.Join(path...)); err != nil {
			return &NFSStatusError{NFSStatusAccess, err}
		} else if !s.IsDir() {
			return &NFSStatusError{NFSStatusNotDir, nil}
		}
	}

	// tailraid: MKDIR is exclusive where the filesystem can decide it (I-238): the Stat
	// above saw this server's view only, and another server over the same volume may have
	// made the folder since, which is NFS3ERR_EXIST as for CREATE GUARDED (I-222)
	if x, ok := fs.(interface {
		MkdirExcl(string, os.FileMode) error
	}); ok {
		if err := x.MkdirExcl(newFolderPath, attrs.Mode(mkdirDefaultMode)); err != nil {
			if st := namespaceStatus(err); st != NFSStatusIO {
				return &NFSStatusError{st, err}
			}
			return &NFSStatusError{NFSStatusAccess, err}
		}
	} else if err := fs.MkdirAll(newFolderPath, attrs.Mode(mkdirDefaultMode)); err != nil {
		return &NFSStatusError{NFSStatusAccess, err}
	}

	fp := userHandle.ToHandle(fs, newFolder)
	changer := userHandle.Change(fs)
	if changer != nil {
		if err := attrs.Apply(changer, fs, newFolderPath); err != nil {
			return &NFSStatusError{NFSStatusIO, err}
		}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	// "handle follows"
	if err := xdr.Write(writer, uint32(1)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, fp); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, newFolder)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := WriteWcc(writer, nil, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
