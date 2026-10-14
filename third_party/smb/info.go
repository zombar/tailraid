// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// What a QUERY_INFO is about.
const (
	infoTypeFile       uint8 = 0x01
	infoTypeFilesystem uint8 = 0x02
	infoTypeSecurity   uint8 = 0x03
)

// File information classes.
const (
	fileBasicInformation       uint8 = 4
	fileStandardInformation    uint8 = 5
	fileInternalInformation    uint8 = 6
	fileEaInformation          uint8 = 7
	fileAccessInformation      uint8 = 8
	fileNameInformation        uint8 = 9
	fileAllInformation         uint8 = 18
	fileAlignmentInformation   uint8 = 17
	filePositionInformation    uint8 = 14
	fileNetworkOpenInformation uint8 = 34
	fileStreamInformation      uint8 = 22
)

// Filesystem information classes.
const (
	fsVolumeInformation    uint8 = 1
	fsSizeInformation      uint8 = 3
	fsDeviceInformation    uint8 = 4
	fsAttributeInformation uint8 = 5
	fsFullSizeInformation  uint8 = 7
)

// queryInfo answers what a client asks about a file or about the share.
func (c *conn) queryInfo(h header, body []byte) ([]byte, error) {
	if len(body) < 40 {
		return nil, fmt.Errorf("smb: QUERY_INFO body of %d bytes is too short", len(body))
	}
	infoType := body[2]
	class := body[3]
	of := c.fileByID(body[24:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	var st filesystem.Stat = pipeStat
	if of.pipe == nil {
		release := of.share.reading()
		var err error
		st, err = of.fs().Stat(of.p())
		if err != nil {
			release()
			return errorResponse(h, statusFor(err, statusObjectNameNotFound)), nil
		}
		defer release()
	}

	var out []byte
	switch infoType {
	case infoTypeFile:
		out = fileInfo(class, of, st)
	case infoTypeFilesystem:
		out = fsInfo(class, of.share, of.ro)
	case infoTypeSecurity:
		// A security descriptor is a whole model this server does not have.
		// Saying so is better than an empty one, which a client reads as "no
		// access for anybody".
		return errorResponse(h, statusNotSupported), nil
	}
	if out == nil {
		return errorResponse(h, statusInvalidInfoClass), nil
	}

	// The answer is bounded by the buffer the client offered (MS-SMB2 3.3.5.20). A
	// variable-length class whose fixed part fits is cut to the buffer with
	// BUFFER_OVERFLOW; a buffer shorter than the fixed part gets nothing. Windows asks
	// for the volume information with room for an empty label whenever a program calls
	// GetFileInformationByHandle, and an answer longer than that buffer was taken for a
	// malformed reply: the call failed with error 58 and nothing in any log.
	status := statusSuccess
	if outMax := int(binary.LittleEndian.Uint32(body[4:])); len(out) > outMax {
		if outMax < fixedPart(infoType, class, len(out)) {
			return errorResponse(h, statusInfoLengthMismatch), nil
		}
		out, status = out[:outMax], statusBufferOverflow
	}

	const bodyLen = 8
	b := append(responseTo(h, status), make([]byte, bodyLen+len(out))...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 9)
	binary.LittleEndian.PutUint16(rb[2:], headerLen+bodyLen)
	binary.LittleEndian.PutUint32(rb[4:], uint32(len(out)))
	copy(rb[bodyLen:], out)
	return b, nil
}

// fixedPart is the size of a class's fixed part in MS-FSCC, n (the whole answer) for a
// class that has no variable part.
func fixedPart(infoType, class uint8, n int) int {
	switch {
	case infoType == infoTypeFile && class == fileNameInformation:
		return 4 // FileNameLength, then the name
	case infoType == infoTypeFile && class == fileStreamInformation:
		return 24 // one entry's fixed fields, then its name
	case infoType == infoTypeFile && class == fileAllInformation:
		return 100 // basic, standard, internal, ea, access, position, mode, alignment, name length
	case infoType == infoTypeFilesystem && class == fsVolumeInformation:
		return 18 // creation time, serial, label length, supports objects, reserved
	case infoType == infoTypeFilesystem && class == fsAttributeInformation:
		return 12 // attributes, longest component, name length
	}
	return n
}

func fileInfo(class uint8, of *openFile, st filesystem.Stat) []byte {
	size := sizeOf(st)
	attrs := attributesOf(of.p(), st, of.ro)
	switch class {
	case fileBasicInformation:
		b := make([]byte, 40)
		writeTimes(b, st)
		binary.LittleEndian.PutUint32(b[32:], attrs)
		return b

	case fileStandardInformation:
		b := make([]byte, 24)
		binary.LittleEndian.PutUint64(b[0:], allocationOf(size))
		binary.LittleEndian.PutUint64(b[8:], size)
		binary.LittleEndian.PutUint32(b[16:], 1) // one link: no hardlink count here
		if of.dir {
			b[21] = 1 // Directory
		}
		return b

	case fileInternalInformation:
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, st.Inode())
		return b

	case fileEaInformation, fileAlignmentInformation:
		// No extended attributes, and byte alignment. Four zero bytes says
		// both, and saying it is what stops a client asking again.
		return make([]byte, 4)

	case filePositionInformation:
		return make([]byte, 8)

	case fileAccessInformation:
		b := make([]byte, 4)
		access := accessAll
		if of.ro {
			access = accessRead
		}
		binary.LittleEndian.PutUint32(b, access)
		return b

	case fileNameInformation:
		name := utf16le(`\` + fsPathToSMB(of.p()))
		b := make([]byte, 4+len(name))
		binary.LittleEndian.PutUint32(b, uint32(len(name)))
		copy(b[4:], name)
		return b

	case fileNetworkOpenInformation:
		b := make([]byte, 56)
		writeTimes(b, st)
		binary.LittleEndian.PutUint64(b[32:], allocationOf(size))
		binary.LittleEndian.PutUint64(b[40:], size)
		binary.LittleEndian.PutUint32(b[48:], attrs)
		return b

	case fileStreamInformation:
		if of.dir {
			// A directory has no data stream, and an empty answer is the
			// right one rather than an error.
			return []byte{}
		}
		name := utf16le("::$DATA")
		b := make([]byte, 24+len(name))
		binary.LittleEndian.PutUint32(b[4:], uint32(len(name)))
		binary.LittleEndian.PutUint64(b[8:], size)
		binary.LittleEndian.PutUint64(b[16:], allocationOf(size))
		copy(b[24:], name)
		return b

	case fileAllInformation:
		// The concatenation the class is defined as: basic, standard,
		// internal, ea, access, position, mode, alignment, name.
		b := make([]byte, 0, 128)
		b = append(b, fileInfo(fileBasicInformation, of, st)...)
		b = append(b, fileInfo(fileStandardInformation, of, st)...)
		b = append(b, fileInfo(fileInternalInformation, of, st)...)
		b = append(b, make([]byte, 4)...) // ea
		b = append(b, fileInfo(fileAccessInformation, of, st)...)
		b = append(b, make([]byte, 8)...) // position
		b = append(b, make([]byte, 4)...) // mode
		b = append(b, make([]byte, 4)...) // alignment
		b = append(b, fileInfo(fileNameInformation, of, st)...)
		return b

	default:
		return nil
	}
}

// The allocation unit the size classes count in: 8 sectors of 512 bytes.
const (
	sectorsPerUnit = 8
	bytesPerSector = 512
	unitBytes      = sectorsPerUnit * bytesPerSector
)

// units is the share's total and available allocation units.
//
// With no capacity known -- go-filesystems has no statfs -- a size is quoted
// that says "there is room" without inventing a total the driver could
// contradict: zero free would make a client refuse every write before
// trying. Available is never more than the total, which a client would
// show as a negative used.
func (sh *share) units() (total, avail uint64) {
	if sh.capacity != nil {
		if t, a := sh.capacity(); t > 0 {
			return t / unitBytes, min(a, t) / unitBytes
		}
	}
	return 1 << 20, 1 << 19
}

func fsInfo(class uint8, sh *share, ro bool) []byte {
	label := sh.name
	if l, ok := sh.fsys.(filesystem.LabelReader); ok {
		if s := l.Label(); s != "" {
			label = s
		}
	}
	switch class {
	case fsVolumeInformation:
		name := utf16le(label)
		b := make([]byte, 18+len(name))
		// ONE timestamp, not four: this structure has a creation time and then
		// the serial number. writeTimes fills four, which ran off the end of an
		// eighteen-byte buffer and took the whole server down -- found by a test,
		// because macOS asks for the size and the attributes and never for this.
		binary.LittleEndian.PutUint64(b[0:], filetime(time.Now()))
		binary.LittleEndian.PutUint32(b[8:], 0x0BADC0DE) // serial number: stable enough to be a serial
		binary.LittleEndian.PutUint32(b[12:], uint32(len(name)))
		copy(b[18:], name)
		return b

	case fsSizeInformation:
		total, avail := sh.units()
		b := make([]byte, 24)
		binary.LittleEndian.PutUint64(b[0:], total) // total units
		binary.LittleEndian.PutUint64(b[8:], avail) // available
		binary.LittleEndian.PutUint32(b[16:], sectorsPerUnit)
		binary.LittleEndian.PutUint32(b[20:], bytesPerSector)
		return b

	case fsFullSizeInformation:
		total, avail := sh.units()
		b := make([]byte, 32)
		binary.LittleEndian.PutUint64(b[0:], total)
		binary.LittleEndian.PutUint64(b[8:], avail)  // the caller's
		binary.LittleEndian.PutUint64(b[16:], avail) // the volume's
		binary.LittleEndian.PutUint32(b[24:], sectorsPerUnit)
		binary.LittleEndian.PutUint32(b[28:], bytesPerSector)
		return b

	case fsDeviceInformation:
		b := make([]byte, 8)
		binary.LittleEndian.PutUint32(b[0:], 0x00000007) // FILE_DEVICE_DISK
		return b

	case fsAttributeInformation:
		name := utf16le("GOFS")
		b := make([]byte, 12+len(name))
		// Caseless comparison and case-preserving names: what the drivers
		// underneath actually do. Unicode on disk is claimed because Go
		// strings are UTF-8 and the names round-trip.
		const (
			caseSensitiveSearch = 0x00000001
			casePreservedNames  = 0x00000002
			unicodeOnDisk       = 0x00000004
			readOnlyVolume      = 0x00080000
		)
		attrs := uint32(casePreservedNames | unicodeOnDisk)
		if ro {
			attrs |= readOnlyVolume
		}
		binary.LittleEndian.PutUint32(b[0:], attrs)
		binary.LittleEndian.PutUint32(b[4:], 255) // longest component
		binary.LittleEndian.PutUint32(b[8:], uint32(len(name)))
		copy(b[12:], name)
		return b

	default:
		return nil
	}
}

// Information classes a client SETS.
const (
	fileBasicInformationSet    uint8 = 4
	fileRenameInformation      uint8 = 10
	fileDispositionInformation uint8 = 13
	fileAllocationInformation  uint8 = 19
	fileEndOfFileInformation   uint8 = 20
)

// setInfo is how a client deletes, renames and truncates: SMB2 has no command
// for any of the three. Deleting is "mark this handle delete-on-close", and
// the file goes when the handle does.
func (c *conn) setInfo(h header, body []byte, msg []byte) ([]byte, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("smb: SET_INFO body of %d bytes is too short", len(body))
	}
	infoType := body[2]
	class := body[3]
	inLen := int(binary.LittleEndian.Uint32(body[4:]))
	inOff := int(binary.LittleEndian.Uint16(body[8:]))
	of := c.fileByID(body[16:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	if of.ro {
		return errorResponse(h, statusMediaWriteProtected), nil
	}
	if inOff < 0 || inLen < 0 || inOff+inLen > len(msg) {
		return nil, fmt.Errorf("smb: the payload is not inside the message")
	}
	in := msg[inOff : inOff+inLen]
	if infoType != infoTypeFile {
		return errorResponse(h, statusNotSupported), nil
	}
	// An end of file changes the one file through its handle, by inode, which a driver
	// that declares concurrent writes serves beside its other calls: a truncate waiting at
	// a cap there holds up no CREATE or CLOSE on the share (I-268). A rename and a
	// delete-on-close change the share's names, and the times are set by path, which a
	// rename on another connection could move under them: those hold the share's lock.
	switch class {
	case fileEndOfFileInformation, fileAllocationInformation:
		defer fileLock(of)()
	default:
		defer of.share.changing()()
	}

	switch class {
	case fileDispositionInformation:
		if len(in) < 1 {
			return errorResponse(h, statusInvalidParameter), nil
		}
		if in[0] != 0 {
			// the delete happens at CLOSE, whose reply carries no failure: a backend
			// that may refuse it is asked now, where the client reads the status
			if err := canDelete(of.fs(), of.p(), of.created); err != nil {
				return errorResponse(h, statusFor(err, statusAccessDenied)), nil
			}
		}
		of.deleteOnClose = in[0] != 0

	case fileRenameInformation:
		// 20 bytes of fixed fields, then the new name in UTF-16.
		if len(in) < 20 {
			return errorResponse(h, statusInvalidParameter), nil
		}
		nameLen := int(binary.LittleEndian.Uint32(in[16:]))
		if 20+nameLen > len(in) {
			return errorResponse(h, statusInvalidParameter), nil
		}
		to, ok := smbPathToFS(fromUTF16le(in[20 : 20+nameLen]))
		if !ok {
			return errorResponse(h, statusObjectNameNotFound), nil
		}
		from := of.p()
		to, ok = renameTarget(of.fs(), to, of.ino)
		if !ok {
			return errorResponse(h, statusObjectPathNotFound), nil // its folder does not exist (I-233)
		}
		if err := renameFor(of, from, to, in[0] != 0); err != nil {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}
		of.share.notify(from, actionRemoved)
		of.share.notify(to, actionAdded)
		of.share.moveHandles(from, to) // every other handle of the file
		of.pmu.Lock()
		of.path = to // and this one, registered or not
		of.pmu.Unlock()

	case fileEndOfFileInformation, fileAllocationInformation:
		if len(in) < 8 {
			return errorResponse(h, statusInvalidParameter), nil
		}
		size := int64(binary.LittleEndian.Uint64(in))
		if class == fileAllocationInformation {
			// an allocation reserves space and is not the end of file: one above the
			// file's size leaves the file as it is, one below cuts it there (MS-FSA
			// 2.1.5.15.1, I-235); as an end of file it padded preallocated files with zeros
			cur, err := c.sizeOf(of)
			if err != nil {
				return errorResponse(h, statusFor(err, statusNotSupported)), nil
			}
			if size >= cur {
				break
			}
		}
		if err := c.truncate(of, size); err != nil {
			return errorResponse(h, statusFor(err, statusNotSupported)), nil
		}
		of.share.notify(of.p(), actionModified)

	case fileBasicInformationSet:
		// Access and write times go to a driver that can set them; attributes,
		// which go-filesystems does not record, are dropped. Accepting the
		// class is deliberate: a client sets it at the end of every copy, and
		// a refusal there is a copy that reports failure after the bytes
		// arrived intact.
		if len(in) < 36 {
			return errorResponse(h, statusInvalidParameter), nil
		}
		if err := c.setTimes(of, in); err != nil {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}

	default:
		return errorResponse(h, statusInvalidInfoClass), nil
	}
	return simpleResponse(h, statusSuccess, 2), nil
}

// setTimes applies the access and write times of a FileBasicInformation. A
// time of 0 means "leave it", -1 "stop updating it" and -2 "resume" (MS-FSCC
// 2.4.7); none of those three touches the driver. A driver without
// MetadataSetter, or without a Stat that knows its times, keeps nothing.
func (c *conn) setTimes(of *openFile, in []byte) error {
	ms, ok := of.fs().(filesystem.MetadataSetter)
	if !ok {
		return nil
	}
	set := func(off int) (time.Time, bool) {
		ft := binary.LittleEndian.Uint64(in[off:])
		if ft == 0 || ft == ^uint64(0) || ft == ^uint64(1) {
			return time.Time{}, false
		}
		return timeOf(ft), true
	}
	atime, setA := set(8)
	mtime, setM := set(16)
	if !setA && !setM {
		return nil
	}
	st, err := of.fs().Stat(of.p())
	if err != nil {
		return err
	}
	ts, ok := st.(Timestamper)
	if !ok {
		return nil
	}
	a, m, _ := ts.Times()
	if !setA {
		atime = a
	}
	if !setM {
		mtime = m
	}
	return ms.Chtimes(of.p(), atime, mtime)
}

// truncate resizes a file through whichever capability the driver has.
// sizeOf is the handle's file size now: its writable file's own when it has one (which
// sees this gateway's unflushed writes), else the driver's Stat.
func (c *conn) sizeOf(of *openFile) (int64, error) {
	if of.w != nil {
		return of.w.Size(), nil
	}
	st, err := of.fs().Stat(of.p())
	if err != nil {
		return 0, err
	}
	return int64(sizeOf(st)), nil //nolint:gosec // a file size fits an int64
}

func (c *conn) truncate(of *openFile, size int64) error {
	if of.w != nil {
		return of.w.Truncate(size)
	}
	if t, ok := of.fs().(filesystem.Truncater); ok {
		return t.Truncate(of.p(), size)
	}
	// Neither capability: do it the only way left, which is to rewrite the
	// file. It is O(size), and it is better than telling a client its copy
	// failed.
	data, err := of.fs().ReadFile(of.p())
	if err != nil {
		return err
	}
	if int64(len(data)) > size {
		data = data[:size]
	} else {
		grown := make([]byte, size)
		copy(grown, data)
		data = grown
	}
	return of.fs().WriteFile(of.p(), data, 0o644)
}

// renameFor renames the file of a handle. With replace an existing destination is
// replaced; without it, it is os.ErrExist and nothing moves, decided by the backend when
// it can (NoReplaceRenamer), else by a look first; the handle's own file under another
// spelling of its name (a case-only rename) is no collision (I-224).
func renameFor(of *openFile, from, to string, replace bool) error {
	fsys := of.fs()
	if replace {
		return fsys.Rename(from, to)
	}
	if r, ok := fsys.(NoReplaceRenamer); ok {
		return r.RenameNoReplace(from, to)
	}
	if st, err := fsys.Stat(to); err == nil && to != from && (of.ino == 0 || st.Inode() != of.ino) {
		return os.ErrExist
	}
	return fsys.Rename(from, to)
}

// fileLock takes the share's lock for a change of one file unless the driver declares
// concurrent writes and the change goes through the handle (I-268); the whole-file
// fallbacks go by name and keep it.
func fileLock(of *openFile) func() {
	if of.w != nil {
		return lockUnlessConcurrent(of.share)
	}
	return of.share.changing()
}
