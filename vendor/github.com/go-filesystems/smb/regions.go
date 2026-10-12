// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"math"
)

// ValidDataReporter is the optional capability of a handle's File that knows its valid
// data length: valid is where the bytes a client wrote end, everything from it to size
// reading as zeros (NTFS's ValidDataLength; a file just created and sized has 0), and
// size is the file's length now. It answers FSCTL_QUERY_FILE_REGIONS, which Windows'
// copy engine asks of every destination it creates: a File without it has the FSCTL
// refused, and that copy engine then deletes the file it has just made (docs/15 §3).
type ValidDataReporter interface {
	ValidData() (valid, size int64, err error)
}

const (
	regionUsageValidCached = 0x00000001 // FILE_REGION_USAGE_VALID_CACHED_DATA
	regionInputLen         = 24         // FILE_REGION_INPUT
	regionOutputHead       = 16         // FILE_REGION_OUTPUT without its regions
	regionInfoLen          = 24         // FILE_REGION_INFO
)

// queryFileRegions is FSCTL_QUERY_FILE_REGIONS, step for step as MS-FSA's pseudocode
// for it has the object store answer, with this server as NTFS (the cached-data usage).
// in is the FILE_REGION_INPUT, empty when the client sent none.
func (c *conn) queryFileRegions(h header, of *openFile, in []byte, maxOut int) []byte {
	if of.dir || of.pipe != nil {
		return errorResponse(h, statusInvalidParameter) // not a data file (MS-FSCC 2.3.56)
	}
	vr, ok := of.f.(ValidDataReporter)
	if !ok {
		return errorResponse(h, statusInvalidDeviceRequest) // optional; not implemented here
	}
	off, length, usage := int64(0), int64(math.MaxInt64), uint32(regionUsageValidCached)
	switch {
	case len(in) == 0:
	case len(in) < regionInputLen:
		return errorResponse(h, statusBufferTooSmall)
	default:
		off = int64(binary.LittleEndian.Uint64(in[0:]))
		length = int64(binary.LittleEndian.Uint64(in[8:]))
		usage = binary.LittleEndian.Uint32(in[16:])
	}
	if length <= 0 || off < 0 || off > math.MaxInt64-length {
		return errorResponse(h, statusInvalidParameter)
	}
	if usage&regionUsageValidCached == 0 {
		return errorResponse(h, statusInvalidParameter)
	}
	if maxOut < regionOutputHead+regionInfoLen { // sizeof(FILE_REGION_OUTPUT) holds Region[1]
		return errorResponse(h, statusBufferTooSmall)
	}
	vdl, eof, err := vr.ValidData()
	if err != nil {
		return errorResponse(h, statusFor(err, statusUnexpectedIoError))
	}
	vdl = max(0, min(vdl, eof))
	if off > eof || (off == eof && eof > 0) {
		return ioctlResponse(h, fsctlQueryFileRegions, nil, of.id, statusSuccess)
	}

	out := make([]byte, regionOutputHead, regionOutputHead+2*regionInfoLen)
	var total, returned uint32
	add := func(o, n int64, u uint32) {
		r := make([]byte, regionInfoLen)
		binary.LittleEndian.PutUint64(r[0:], uint64(o))
		binary.LittleEndian.PutUint64(r[8:], uint64(n))
		binary.LittleEndian.PutUint32(r[16:], u)
		out = append(out, r...)
		total++
		returned++
	}
	status := statusSuccess
	if off >= vdl {
		add(off, min(length, eof-off), 0)
	} else {
		first := min(vdl-off, length)
		add(off, first, usage)
		if vdl < eof && first < length {
			if len(out)+regionInfoLen > maxOut {
				total++
				status = statusBufferOverflow
			} else {
				add(vdl, min(length-first, eof-vdl), 0)
			}
		}
	}
	binary.LittleEndian.PutUint32(out[4:], total)
	binary.LittleEndian.PutUint32(out[8:], returned)
	return ioctlResponse(h, fsctlQueryFileRegions, out, of.id, status)
}
