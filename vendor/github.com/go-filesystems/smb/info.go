// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
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
		st, err = of.share.fsys.Stat(of.path)
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

	const bodyLen = 8
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+len(out))...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 9)
	binary.LittleEndian.PutUint16(rb[2:], headerLen+bodyLen)
	binary.LittleEndian.PutUint32(rb[4:], uint32(len(out)))
	copy(rb[bodyLen:], out)
	return b, nil
}

func fileInfo(class uint8, of *openFile, st filesystem.Stat) []byte {
	size := sizeOf(st)
	attrs := attributesOf(of.path, st, of.ro)
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
		name := utf16le(`\` + fsPathToSMB(of.path))
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
	defer of.share.changing()()

	switch class {
	case fileDispositionInformation:
		if len(in) < 1 {
			return errorResponse(h, statusInvalidParameter), nil
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
		if err := of.share.fsys.Rename(of.path, to); err != nil {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}
		of.share.notify(of.path, actionRemoved)
		of.share.notify(to, actionAdded)
		of.path = to

	case fileEndOfFileInformation, fileAllocationInformation:
		if len(in) < 8 {
			return errorResponse(h, statusInvalidParameter), nil
		}
		size := int64(binary.LittleEndian.Uint64(in))
		if err := c.truncate(of, size); err != nil {
			return errorResponse(h, statusFor(err, statusNotSupported)), nil
		}
		of.share.notify(of.path, actionModified)

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
	ms, ok := of.share.fsys.(filesystem.MetadataSetter)
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
	st, err := of.share.fsys.Stat(of.path)
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
	return ms.Chtimes(of.path, atime, mtime)
}

// truncate resizes a file through whichever capability the driver has.
func (c *conn) truncate(of *openFile, size int64) error {
	if of.w != nil {
		return of.w.Truncate(size)
	}
	if t, ok := of.share.fsys.(filesystem.Truncater); ok {
		return t.Truncate(of.path, size)
	}
	// Neither capability: do it the only way left, which is to rewrite the
	// file. It is O(size), and it is better than telling a client its copy
	// failed.
	data, err := of.share.fsys.ReadFile(of.path)
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
	return of.share.fsys.WriteFile(of.path, data, 0o644)
}
