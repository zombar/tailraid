// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
)

// The control codes this server answers. Everything else is refused by name.
const (
	fsctlValidateNegotiateInfo uint32 = 0x00140204
	fsctlDfsGetReferrals       uint32 = 0x00060194
	fsctlPipeTransceive        uint32 = 0x0011C017
	fsctlQueryFileRegions      uint32 = 0x00090284 // MS-FSCC 2.3.55
)

// ioctl answers the one control code a 3.x client insists on.
//
// FSCTL_VALIDATE_NEGOTIATE_INFO exists because the dialect exchange happens
// BEFORE there is a session, so nothing signs it and a man in the middle could
// have talked both sides down to a weaker dialect. Once the session exists, the
// client asks the server to repeat what it said -- signed this time -- and
// compares. Windows treats a failure as an attack and drops the connection, so
// "not implemented" is not a safe answer for it the way it is for the rest.
//
// The answer must be what was actually negotiated, which is why the server's
// GUID is fixed for its lifetime and the client's own numbers are remembered.
func (c *conn) ioctl(h header, body []byte, msg []byte) ([]byte, error) {
	if len(body) < 56 {
		return nil, fmt.Errorf("smb: IOCTL body of %d bytes is too short", len(body))
	}
	code := binary.LittleEndian.Uint32(body[4:])
	inOff := int(binary.LittleEndian.Uint32(body[24:]))
	inLen := int(binary.LittleEndian.Uint32(body[28:]))
	maxOut := int(binary.LittleEndian.Uint32(body[44:]))
	// MS-SMB2 §3.3.5.15, in its order: only FSCTLs are served; an input buffer must sit
	// after the fixed part, 8-aligned and inside the message (I-228).
	if binary.LittleEndian.Uint32(body[48:])&ioctlIsFsctl == 0 {
		return errorResponse(h, statusNotSupported), nil
	}
	if inLen != 0 && (inOff < headerLen+ioctlRequestFixed || inOff%8 != 0 || inOff > len(msg) || inOff+inLen > len(msg)) {
		return errorResponse(h, statusInvalidParameter), nil
	}
	b := c.fsctl(h, code, body, msg, inOff, inLen)
	return clampOutput(b, maxOut), nil
}

const (
	ioctlIsFsctl      = 0x00000001 // SMB2_0_IOCTL_IS_FSCTL
	ioctlRequestFixed = 56         // the IOCTL request without its buffer

	fileDeviceFileSystem = 0x0009 // the device type of every MS-FSCC 2.3 FSCTL
)

// fsctl answers one FSCTL whose input, when it has one, is msg[inOff:inOff+inLen].
func (c *conn) fsctl(h header, code uint32, body, msg []byte, inOff, inLen int) []byte {
	switch code {
	case fsctlValidateNegotiateInfo:
		if inOff < 0 || inLen < 24 || inOff+inLen > len(msg) {
			return errorResponse(h, statusInvalidParameter)
		}
		in := msg[inOff : inOff+inLen]
		count := int(binary.LittleEndian.Uint16(body[0:]))
		_ = count
		// What the client says it sent. A mismatch means the negotiate it
		// remembers is not the one that happened.
		theirCaps := binary.LittleEndian.Uint32(in[0:])
		theirMode := binary.LittleEndian.Uint16(in[20:])
		if theirCaps != c.clientCapabilities || theirMode != c.clientSecurityMode {
			// ACCESS_DENIED rather than a polite refusal: the client is asking
			// whether it was tampered with, and the honest answer is that what
			// it remembers is not what arrived.
			return errorResponse(h, statusAccessDenied)
		}
		out := make([]byte, 24)
		binary.LittleEndian.PutUint32(out[0:], capabilitiesFor(c.dialect)) // as NEGOTIATE said
		copy(out[4:], c.srv.guid[:])
		binary.LittleEndian.PutUint16(out[20:], signingEnabled)
		binary.LittleEndian.PutUint16(out[22:], c.dialect)
		return ioctlResponse(h, code, out, [16]byte{}, statusSuccess)

	case fsctlPipeTransceive:
		// Write and read in one round trip, which is how a remote procedure
		// call is made over a pipe: the call goes in the input, the reply
		// comes back in the output. It is the only way `smbutil view` and the
		// Finder ask what shares there are.
		of := c.fileByID(body[8:])
		if of == nil || of.pipe == nil {
			return errorResponse(h, statusFileClosed)
		}
		if inOff < 0 || inLen < 0 || inOff+inLen > len(msg) {
			return errorResponse(h, statusInvalidParameter)
		}
		reply := of.pipe.answer(c.srv, c.session(h).user, msg[inOff:inOff+inLen])
		if reply == nil {
			// Not something this pipe speaks. INVALID_PARAMETER rather than a
			// fault: a fault is an answer within DCE/RPC, and this was not
			// DCE/RPC at all.
			return errorResponse(h, statusInvalidParameter)
		}
		of.pipe.out = reply
		out, more := of.pipe.take(int(binary.LittleEndian.Uint32(body[44:])))
		status := statusSuccess
		if more {
			// As much as fits, and a warning that says so. The client READs
			// the tail from the pipe, which is why it stays on the handle.
			status = statusBufferOverflow
		}
		return ioctlResponse(h, code, out, of.id, status)

	case fsctlSrvRequestResumeKey:
		of := c.fileByID(body[8:])
		if of == nil {
			return errorResponse(h, statusFileClosed)
		}
		return c.resumeKey(h, of)

	case fsctlSrvCopyChunk, fsctlSrvCopyChunkWrite:
		of := c.fileByID(body[8:])
		if of == nil {
			return errorResponse(h, statusFileClosed)
		}
		if inOff < 0 || inLen < 0 || inOff+inLen > len(msg) {
			return errorResponse(h, statusInvalidParameter)
		}
		return c.copyChunk(h, code, of, msg[inOff:inOff+inLen])

	case fsctlDfsGetReferrals:
		// There is no distributed filesystem here, and saying so is what stops
		// a client looking for one.
		return errorResponse(h, statusNotSupported)

	case fsctlQueryFileRegions:
		of := c.fileByID(body[8:])
		if of == nil {
			return errorResponse(h, statusFileClosed)
		}
		var in []byte
		if inLen > 0 {
			in = msg[inOff : inOff+inLen]
		}
		return c.queryFileRegions(h, of, in, int(binary.LittleEndian.Uint32(body[44:])))

	default:
		// An FSCTL of the file system's own (MS-FSCC 2.3) is allowed but not supported on
		// this one; any other is not allowed here (MS-SMB2 §3.3.5.15, §3.3.5.15.8).
		if code>>16 == fileDeviceFileSystem {
			return errorResponse(h, statusInvalidDeviceRequest)
		}
		return errorResponse(h, statusNotSupported)
	}
}

// clampOutput keeps an IOCTL answer within the client's MaxOutputResponse: an answer
// with more output is cut to what fits and says so with STATUS_BUFFER_OVERFLOW
// (MS-SMB2 §3.3.5.15).
func clampOutput(b []byte, maxOut int) []byte {
	if len(b) < headerLen+48 || binary.LittleEndian.Uint16(b[headerLen:]) != 49 {
		return b // an error response carries no output
	}
	rb := b[headerLen:]
	off := int(binary.LittleEndian.Uint32(rb[32:]))
	n := int(binary.LittleEndian.Uint32(rb[36:]))
	if n <= maxOut {
		return b
	}
	binary.LittleEndian.PutUint32(rb[36:], uint32(maxOut))
	binary.LittleEndian.PutUint32(b[offStatus:], statusBufferOverflow)
	return b[:off+maxOut]
}

func ioctlResponse(h header, code uint32, out []byte, id [16]byte, status uint32) []byte {
	const bodyLen = 48
	b := append(responseTo(h, status), make([]byte, bodyLen+len(out))...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 49)
	binary.LittleEndian.PutUint32(rb[4:], code)
	// The file id is all zeros for a control code about the CONNECTION -- and
	// the handle's own for one about something opened on it, which a client
	// matches against what it sent.
	copy(rb[8:], id[:])
	//
	// OUTPUT at 32 and 36, not 24 and 28 -- those are the INPUT offset and
	// count, and a response that puts the payload's size there leaves the
	// output count zero. The Linux kernel reads exactly that field and says
	// "Invalid protocol negotiate response size: 0", then refuses the mount.
	binary.LittleEndian.PutUint32(rb[32:], headerLen+bodyLen)
	binary.LittleEndian.PutUint32(rb[36:], uint32(len(out)))
	copy(rb[bodyLen:], out)
	return b
}
