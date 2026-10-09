// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb

import "encoding/binary"

// capLargeMTU is SMB2_GLOBAL_CAP_LARGE_MTU (MS-SMB2 2.2.4): "the server
// supports multi-credit operations". Without it a client holds every READ
// and WRITE to 64 KiB whatever MaxReadSize says -- go-smb2 and the Linux
// kernel client both do -- and a megabyte costs sixteen round trips.
const capLargeMTU uint32 = 0x00000004

// capabilitiesFor is what the server claims for a dialect, in the NEGOTIATE
// response and again in VALIDATE_NEGOTIATE_INFO, which the client compares
// with the first and disconnects over if they differ. Multi-credit does not
// exist in 2.0.2, nor in the wildcard answer to the legacy greeting.
func capabilitiesFor(dialect uint16) uint32 {
	if dialect >= dialect210 && dialect != dialectWildcard {
		return capLargeMTU
	}
	return 0
}

// creditsNeeded is MS-SMB2 3.1.5.2's formula: one credit per 64 KiB of the
// larger of what the request sends and the most its response may carry.
//
// It is a uint32 on purpose. A size near 4 GiB needs 65536 credits, which
// is 0 as a uint16: a request asking for that much while paying one credit
// passed the check below, until v0.6.1.
func creditsNeeded(size uint32) uint32 {
	if size == 0 {
		return 1
	}
	return (size-1)/65536 + 1
}

// payload is what a body of n bytes carries past a fixed part of fixed bytes.
func payload(n, fixed int) uint32 {
	if n <= fixed {
		return 0
	}
	return uint32(n - fixed)
}

// creditChargeTooLow applies MS-SMB2 3.3.5.2.5 to one request on a
// connection that supports multi-credit operations: a request whose
// CreditCharge does not pay for its payload, or for the largest response it
// asks for, is failed with STATUS_INVALID_PARAMETER. Without the check, a
// client could spend one credit on a megabyte, and the credit window that
// bounds what one connection may have in flight would bound nothing.
//
// body is the request after its header; a body too short for the field read
// here is left to the command, which refuses it on its own terms.
func creditChargeTooLow(h header, body []byte) bool {
	size := uint32(len(body))
	at := func(off int) uint32 {
		if len(body) < off+4 {
			return 0
		}
		return binary.LittleEndian.Uint32(body[off:])
	}
	switch h.command {
	case cmdWrite:
		// The charge pays for the payload (MS-SMB2 3.1.5.2: SendPayloadSize is the
		// data, not the request), so a WRITE of exactly 64 KiB -- what Windows and
		// the Linux client send -- costs one credit, not two for its 48-byte header.
		size = max(at(4), payload(len(body), 48)) // Length, or what the body holds past the fixed part
	case cmdSetInfo:
		size = max(at(4), payload(len(body), 32)) // BufferLength
	case cmdRead:
		size = max(size, at(4)) // Length
	case cmdQueryDirectory:
		size = max(size, at(28)) // OutputBufferLength
	case cmdQueryInfo, cmdChangeNotify:
		size = max(size, at(4)) // OutputBufferLength
	case cmdIoctl:
		size = max(size, at(32), at(44)) // MaxInputResponse, MaxOutputResponse
	}
	// WRITE and SET_INFO carry their data in the body itself, which len(body)
	// already measures.
	if h.creditCharge == 0 {
		return size > 65536
	}
	return creditsNeeded(size) > uint32(h.creditCharge)
}
