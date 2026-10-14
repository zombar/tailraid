package nfs

import (
	"bytes"
	"context"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/xdr"
)

func onAccess(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = opAttrErrorFormatter
	roothandle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs, path, err := userHandle.FromHandle(roothandle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	mask, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		mask = mask & (1 | 2 | 0x20)
	}
	if rr, ok := fs.(RoleReporter); ok {
		read, write, err := rr.Role(fs.Join(path...))
		if err != nil {
			return &NFSStatusError{NFSStatusAccess, err}
		}
		if !write {
			mask &= 1 | 2 | 0x20 // READ, LOOKUP, EXECUTE
		}
		if !read {
			mask &= 2 | 0x20 // LOOKUP and EXECUTE alone: on the way to a role
		}
	}

	if err := xdr.Write(writer, mask); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
