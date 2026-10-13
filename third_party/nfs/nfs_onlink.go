package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/xdr"
)

// onLink answers LINK (RFC 1813 §3.3.15). tailraid: upstream decoded SYMLINK's arguments
// and wrote a CREATE-style reply; LINK3args is the file's handle and the new name's
// diropargs3, and both LINK3resok and LINK3resfail are the file's post_op_attr and the
// directory's wcc_data (I-245). A backing filesystem without UnixChange (tailraid's: hard
// links are out of scope) answers NFS3ERR_NOTSUPP.
func onLink(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = linkErrorFormatter(nil, nil, nil)
	file, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	obj := DirOpArg{}
	if err := xdr.Read(w.req.Body, &obj); err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	fs, path, err := userHandle.FromHandle(obj.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	_, fpath, err := userHandle.FromHandle(file) // one export: the same filesystem
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	w.errorFmt = linkErrorFormatter(fs, path, fpath)
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}
	if len(string(obj.Filename)) > PathNameMax {
		return &NFSStatusError{NFSStatusNameTooLong, os.ErrInvalid}
	}
	cos, ok := userHandle.Change(fs).(UnixChange)
	if !ok {
		return &NFSStatusError{NFSStatusNotSupp, os.ErrInvalid}
	}
	newFilePath := fs.Join(append(path, string(obj.Filename))...)
	if _, err := fs.Lstat(newFilePath); err == nil {
		return &NFSStatusError{NFSStatusExist, os.ErrExist}
	}
	if err := cos.Link(fs.Join(fpath...), newFilePath); err != nil {
		return &NFSStatusError{NFSStatusAccess, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, fpath)); err != nil {
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

// linkErrorFormatter is LINK3resfail: the file's attributes and the directory's wcc_data,
// empty until the handles resolved.
func linkErrorFormatter(fs billy.Filesystem, dir, file []string) func(err error) RPCError {
	return func(err error) RPCError {
		var b bytes.Buffer
		var fattr, dattr *FileAttribute
		if fs != nil {
			fattr, dattr = tryStat(fs, file), tryStat(fs, dir)
		}
		if WritePostOpAttrs(&b, fattr) != nil || WriteWcc(&b, nil, dattr) != nil {
			return wccDataErrorFormatter(err)
		}
		return errFormatterWithBody(b.Bytes())(err)
	}
}
