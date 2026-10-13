package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/xdr"
)

const (
	createModeUnchecked = 0
	createModeGuarded   = 1
	createModeExclusive = 2
)

func onCreate(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter
	obj := DirOpArg{}
	err := xdr.Read(w.req.Body, &obj)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	how, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	var attrs *SetFileAttributes
	var verf [8]byte
	if how == createModeUnchecked || how == createModeGuarded {
		sattr, err := ReadSetFileAttributes(w.req.Body)
		if err != nil {
			return &NFSStatusError{NFSStatusInval, err}
		}
		attrs = sattr
	} else if how == createModeExclusive {
		// tailraid: EXCLUSIVE as RFC 1813 §3.3.8 has it (I-231). The verifier is kept in
		// the new file's times, as other servers keep it, so a retransmission finds its
		// own file; the client sets the real attributes with a SETATTR afterwards.
		if err := xdr.Read(w.req.Body, &verf); err != nil {
			return &NFSStatusError{NFSStatusInval, err}
		}
	} else {
		// invalid
		return &NFSStatusError{NFSStatusNotSupp, os.ErrInvalid}
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
		return &NFSStatusError{NFSStatusNameTooLong, nil}
	}

	newFile := append(path, string(obj.Filename))
	newFilePath := fs.Join(newFile...)
	if s, err := fs.Stat(newFilePath); err == nil {
		if s.IsDir() {
			return &NFSStatusError{NFSStatusExist, nil}
		}
		if how == createModeGuarded {
			return &NFSStatusError{NFSStatusExist, os.ErrPermission}
		}
		if how == createModeExclusive {
			if !holdsVerifier(s, verf) {
				return &NFSStatusError{NFSStatusExist, os.ErrExist}
			}
			return createReply(w, userHandle, fs, path, newFile, newFilePath)
		}
	} else {
		if s, err := fs.Stat(fs.Join(path...)); err != nil {
			return &NFSStatusError{NFSStatusAccess, err}
		} else if !s.IsDir() {
			return &NFSStatusError{NFSStatusNotDir, nil}
		}
	}

	var file billy.File
	if how == createModeGuarded || how == createModeExclusive {
		// tailraid: GUARDED and EXCLUSIVE are exclusive. The Stat above saw this server's
		// view only; another server over the same volume may have made the name since,
		// and the filesystem's O_EXCL decides (docs/07, I-222). An EXCLUSIVE retransmission
		// whose first attempt another server answered finds its own verifier.
		file, err = fs.OpenFile(newFilePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			if s, serr := fs.Stat(newFilePath); how == createModeExclusive && serr == nil && holdsVerifier(s, verf) {
				return createReply(w, userHandle, fs, path, newFile, newFilePath)
			}
			return &NFSStatusError{NFSStatusExist, err}
		}
	} else {
		file, err = fs.Create(newFilePath)
	}
	if err != nil {
		Log.Errorf("Error Creating: %v", err)
		return &NFSStatusError{NFSStatusAccess, err}
	}
	if err := file.Close(); err != nil {
		Log.Errorf("Error Creating: %v", err)
		return &NFSStatusError{NFSStatusAccess, err}
	}

	changer := userHandle.Change(fs)
	if how == createModeExclusive {
		t := verifierTime(verf)
		if changer == nil {
			return &NFSStatusError{NFSStatusNotSupp, os.ErrPermission}
		}
		if err := changer.Chtimes(newFilePath, t, t); err != nil {
			return &NFSStatusError{NFSStatusIO, err}
		}
	} else if err := attrs.Apply(changer, fs, newFilePath); err != nil {
		Log.Errorf("Error applying attributes: %v\n", err)
		return &NFSStatusError{NFSStatusIO, err}
	}
	return createReply(w, userHandle, fs, path, newFile, newFilePath)
}

// verifierTime is the time an EXCLUSIVE create keeps its createverf3 in (I-231).
func verifierTime(verf [8]byte) time.Time {
	return time.Unix(0, int64(binary.BigEndian.Uint64(verf[:]))) //nolint:gosec // the verifier's bits, as they are
}

// holdsVerifier reports whether an existing file is the one an EXCLUSIVE create with
// verf made: its modification time still carries the verifier.
func holdsVerifier(s os.FileInfo, verf [8]byte) bool {
	return !s.IsDir() && s.ModTime().Equal(verifierTime(verf))
}

// createReply answers a CREATE with the new file's handle and attributes.
func createReply(w *response, userHandle Handler, fs billy.Filesystem, path, newFile []string, newFilePath string) error {
	fp := userHandle.ToHandle(fs, newFile)
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
	if err := WritePostOpAttrs(writer, tryStat(fs, newFile)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	// dir_wcc (we don't include pre_op_attr)
	if err := xdr.Write(writer, uint32(0)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
