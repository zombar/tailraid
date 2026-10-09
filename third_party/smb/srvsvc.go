// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"slices"
	"strings"
)

// What shares are there?
//
// A client that was told a share name can mount it -- that is what
// TREE_CONNECT does. A client that was told only the SERVER has to ask, and
// the question is not an SMB command: it is a remote procedure call, over
// DCE/RPC, over a named pipe, over SMB. `smbutil view`, `net view`,
// `smbclient -L` and the Finder's server window are all this one call.
//
// So this file is three formats deep:
//
//	SMB2 IOCTL FSCTL_PIPE_TRANSCEIVE   the envelope: bytes in, bytes out
//	DCE/RPC (C706)                     bind, then request and response by opnum
//	NDR                                how the answer is laid out
//
// None of it is general. There is one interface (srvsvc), one call
// (NetrShareEnum, opnum 15) and the two information levels a client actually
// asks for. Everything else is refused BY NAME -- a fault carrying a reason,
// not silence -- because a client that gets no answer waits, and one that gets
// "no" carries on.
const (
	rpcVersion      uint8 = 5
	rpcVersionMinor uint8 = 0

	rpcTypeRequest  uint8 = 0
	rpcTypeResponse uint8 = 2
	rpcTypeFault    uint8 = 3
	rpcTypeBind     uint8 = 11
	rpcTypeBindAck  uint8 = 12

	rpcFlagFirst uint8 = 0x01
	rpcFlagLast  uint8 = 0x02

	// The data representation: little-endian, IEEE floats, ASCII characters.
	// It is a field rather than an assumption because DCE/RPC lets the CALLER
	// choose the byte order, and a big-endian client would set it differently.
	// This server answers in the order it was asked in only because every
	// client asks in this one.
	rpcLittleEndian uint32 = 0x00000010

	opNetShareEnum uint16 = 15

	// Faults, from C706 appendix E.
	faultOpRange  uint32 = 0x1C010002 // no such operation
	faultProtoErr uint32 = 0x1C01000B // a call before a bind
)

// The transfer syntax every one of these calls uses: NDR version 2. It is
// named in the bind and named back in the acknowledgement, and a client checks
// that the server picked the one it offered.
var ndrSyntax = [16]byte{
	0x04, 0x5D, 0x88, 0x8A, 0xEB, 0x1C, 0xC9, 0x11,
	0x9F, 0xE8, 0x08, 0x00, 0x2B, 0x10, 0x48, 0x60,
}

// Share types, from MS-SRVS. A disk share is zero, which is why a server that
// forgets to fill this in still looks right -- and why IPC$ must be filled in
// or a client tries to mount it as a disk.
const (
	shareTypeDisktree uint32 = 0x00000000
	shareTypeIPC      uint32 = 0x00000003
	shareTypeSpecial  uint32 = 0x80000000
)

// A shareEntry is one row of the answer.
type shareEntry struct {
	name    string
	comment string
	kind    uint32
}

// sharesFor is what this user may connect to, in a stable order.
//
// The order is sorted rather than whatever the map hands back, because this is
// a LIST a person reads: one that reshuffles itself between two runs looks
// broken, and a test written against it would pass or fail by luck.
//
// It is filtered by user for the same reason TREE_CONNECT refuses: a share
// somebody may not use should not be in their list. A name is not a secret,
// but offering it and then refusing it is worse than not offering it.
func (s *Server) sharesFor(user string) []shareEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]shareEntry, 0, len(s.shares)+1)
	for _, sh := range s.shares {
		if !sh.mayConnect(user) {
			continue
		}
		out = append(out, shareEntry{name: sh.name, kind: shareTypeDisktree})
	}
	slices.SortFunc(out, func(a, b shareEntry) int { return strings.Compare(a.name, b.name) })
	// IPC$ is last and is announced the way Windows announces it. It is in the
	// list because it is real -- the client is talking to this server THROUGH
	// it -- and a client that sees it marked IPC does not try to mount it.
	return append(out, shareEntry{name: "IPC$", comment: "Remote IPC", kind: shareTypeIPC | shareTypeSpecial})
}

// rpcCall is a request taken apart far enough to answer it.
type rpcCall struct {
	kind             uint8
	callID           uint32
	opnum            uint16
	maxXmit, maxRecv uint16
	stub             []byte
}

// parseRPC reads the common header. A packet that is not DCE/RPC version 5.0
// is not answered at all: this is a pipe only this one protocol speaks.
func parseRPC(b []byte) (rpcCall, bool) {
	if len(b) < 24 || b[0] != rpcVersion || b[1] != rpcVersionMinor {
		return rpcCall{}, false
	}
	call := rpcCall{
		kind:   b[2],
		callID: binary.LittleEndian.Uint32(b[12:]),
		// 24, because a request carries the eight-byte stub header first. A
		// bind has no stub and this slice is not read for one.
		stub: b[24:],
	}
	switch call.kind {
	case rpcTypeRequest:
		call.opnum = binary.LittleEndian.Uint16(b[22:])
	case rpcTypeBind:
		call.maxXmit = binary.LittleEndian.Uint16(b[16:])
		call.maxRecv = binary.LittleEndian.Uint16(b[18:])
		if call.maxXmit == 0 || call.maxRecv == 0 {
			return rpcCall{}, false
		}
	}
	return call, true
}

// netShareEnumLevel reads the one argument of the call that matters.
//
// The stub is a pointer to the server's own name -- which this server ignores,
// because it has one identity and answers on it however it was addressed --
// and then the information level. The name is a conformant varying string, so
// finding the level means walking past a length that came from the client:
// it is checked against what actually arrived rather than trusted.
func netShareEnumLevel(stub []byte) (uint32, bool) {
	if len(stub) < 4 {
		return 0, false
	}
	off := 4
	if binary.LittleEndian.Uint32(stub[0:]) != 0 {
		if len(stub) < 16 {
			return 0, false
		}
		chars := binary.LittleEndian.Uint32(stub[12:])
		if chars > uint32(len(stub)) {
			return 0, false
		}
		off = 16 + int(chars)*2
		off = (off + 3) &^ 3
	}
	if off+4 > len(stub) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(stub[off:]), true
}

// rpcHeader writes the sixteen bytes EVERY packet starts with, and then the
// packet's own header follows it.
//
// Sixteen, not twenty-four. The four fields at 16 -- allocation hint, context
// id, opnum or cancel count -- look like part of the common header because
// every request and response has them, but they belong to the packet type: a
// bind puts its fragment sizes there instead. A server that treats the header
// as twenty-four bytes writes a bind acknowledgement whose sizes land eight
// bytes late, and a client reads its own maximum fragment as the association
// group.
func rpcHeader(kind uint8, callID uint32, restLen int) []byte {
	b := make([]byte, 16)
	b[0] = rpcVersion
	b[1] = rpcVersionMinor
	b[2] = kind
	b[3] = rpcFlagFirst | rpcFlagLast
	binary.LittleEndian.PutUint32(b[4:], rpcLittleEndian)
	binary.LittleEndian.PutUint16(b[8:], uint16(16+restLen))
	binary.LittleEndian.PutUint16(b[10:], 0) // no authentication trailer
	binary.LittleEndian.PutUint32(b[12:], callID)
	return b
}

// stubHeader is the eight bytes a request or a response carries between the
// common header and the payload: how much is coming, which presentation
// context it belongs to, and whether it was cancelled.
func stubHeader(stubLen int) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:], uint32(stubLen)) // allocation hint
	binary.LittleEndian.PutUint16(b[4:], 0)               // context id
	b[6] = 0                                              // cancel count
	return b
}

// bindAck accepts the one interface this pipe has.
//
// The secondary address is the pipe's own name, and it is a counted C string:
// the length INCLUDES the terminator, and the result list that follows is
// aligned to four bytes from the start of the packet. Getting that padding
// wrong is invisible in the length and fatal to the parse.
func bindAck(callID uint32, maxXmit, maxRecv uint16) []byte {
	const addr = `\PIPE\srvsvc`
	rest := make([]byte, 0, 56)
	rest = binary.LittleEndian.AppendUint16(rest, maxXmit)
	rest = binary.LittleEndian.AppendUint16(rest, maxRecv)
	// The association group. Windows hands out a number here and clients keep
	// it; a fixed one is honest for a server that has exactly one association
	// per pipe and never asks a client to name it again.
	rest = binary.LittleEndian.AppendUint32(rest, 0x00001063)
	rest = binary.LittleEndian.AppendUint16(rest, uint16(len(addr)+1))
	rest = append(rest, addr...)
	rest = append(rest, 0)
	for (len(rest)+16)%4 != 0 {
		rest = append(rest, 0)
	}
	rest = append(rest, 1, 0, 0, 0)                  // one result, then reserved
	rest = binary.LittleEndian.AppendUint16(rest, 0) // acceptance
	rest = binary.LittleEndian.AppendUint16(rest, 0) // and so no reason
	rest = append(rest, ndrSyntax[:]...)             // the syntax it asked for
	rest = binary.LittleEndian.AppendUint32(rest, 2) // NDR version 2
	return append(rpcHeader(rpcTypeBindAck, callID, len(rest)), rest...)
}

// rpcFault says no, with a reason, in the shape a client expects.
func rpcFault(callID uint32, status uint32) []byte {
	stub := make([]byte, 8)
	binary.LittleEndian.PutUint32(stub[0:], status)
	rest := append(stubHeader(len(stub)), stub...)
	b := append(rpcHeader(rpcTypeFault, callID, len(rest)), rest...)
	b[3] |= 0x20 // did not execute: the client may retry elsewhere
	return b
}

// An ndrWriter lays out the answer. NDR is positional and self-referential --
// a pointer is a number that says "something follows later" -- so the writer
// exists to keep the two halves in step rather than to abstract anything.
type ndrWriter struct{ b []byte }

func (w *ndrWriter) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }

// str writes a conformant and varying string: the three counts, the UTF-16
// characters, the terminator, and the padding back to a four-byte boundary.
// The counts are in CHARACTERS and include the terminator.
func (w *ndrWriter) str(s string) {
	chars := utf16le(s)
	n := uint32(len(chars)/2 + 1)
	w.u32(n) // maximum count
	w.u32(0) // offset
	w.u32(n) // actual count
	w.b = append(w.b, chars...)
	w.b = append(w.b, 0, 0) // the terminator, which the counts include
	for len(w.b)%4 != 0 {
		w.b = append(w.b, 0)
	}
}

// netShareEnumReply answers NetrShareEnum.
//
// The shape, for level 1 (offsets from the start of the stub):
//
//	 0  level                    the client asked for it and gets it back
//	 4  the union arm            level again, as the union's discriminant
//	 8  pointer to the container
//	12  entries read             <- what a client reads as the count
//	16  pointer to the array
//	20  maximum count            the conformant array's own header
//	24  the array: one twelve-byte row per share
//	    then every string, in row order: name, comment, name, comment...
//	    then total entries, the resume handle, and the return value
//
// Level 0 is the same with four-byte rows and no comments. Any other level is
// answered with WERR_INVALID_LEVEL and an empty container, which is what the
// specification says and what a client turns into "this server cannot tell me
// that" rather than a hang.
func netShareEnumStub(level uint32, entries []shareEntry) []byte {
	const (
		werrOK           uint32 = 0
		werrInvalidLevel uint32 = 124
	)
	w := &ndrWriter{}
	if level != 0 && level != 1 {
		w.u32(level)
		w.u32(level)
		w.u32(0) // a null container: there is nothing to point at
		w.u32(0) // total entries
		w.u32(0) // resume handle
		w.u32(werrInvalidLevel)
		return w.b
	}

	n := uint32(len(entries))
	w.u32(level)
	w.u32(level)
	w.u32(0x00020000) // a pointer to the container: any non-zero id will do
	w.u32(n)
	w.u32(0x00020004) // and one to the array
	w.u32(n)
	// The rows first, with a pointer where each string will be, then the
	// strings themselves in the same order. A client walks them in exactly
	// that order and never looks at the pointer values.
	id := uint32(0x00020008)
	for _, e := range entries {
		w.u32(id)
		id += 4
		if level == 1 {
			w.u32(e.kind)
			w.u32(id)
			id += 4
		}
	}
	for _, e := range entries {
		w.str(e.name)
		if level == 1 {
			w.str(e.comment)
		}
	}
	w.u32(n)      // total entries: the same, because nothing is being paged
	w.u32(0)      // the resume handle stays null, as it arrived
	w.u32(werrOK) // and the call worked
	return w.b
}

// rpcResponse cuts a stub into fragments the client said it could receive, each
// with its own header.
//
// This is not an optimisation. A reply too big for one fragment is SENT as
// several, and a client reads them one at a time: it takes the first one's
// bytes, then expects the next read to begin with a header again. A server
// that hands back one long stream instead -- the same bytes, no headers -- is
// read as a broken response, which is exactly what happened here with sixty
// shares and a client offering a kilobyte.
//
// The first fragment says FIRST and the last says LAST; a reply that fits says
// both, which is the ordinary case and the reason this can go unnoticed.
func rpcResponse(callID uint32, stub []byte, maxFrag int) [][]byte {
	const headers = 24
	if maxFrag < headers+8 {
		// A client cannot mean this. The floor is enough to carry a header and
		// something, so the loop below always makes progress.
		maxFrag = 1024
	}
	room := maxFrag - headers
	var out [][]byte
	for first := true; len(stub) > 0 || first; first = false {
		n := min(room, len(stub))
		rest := append(stubHeader(len(stub)), stub[:n]...)
		frag := append(rpcHeader(rpcTypeResponse, callID, len(rest)), rest...)
		frag[3] = 0
		if first {
			frag[3] |= rpcFlagFirst
		}
		stub = stub[n:]
		if len(stub) == 0 {
			frag[3] |= rpcFlagLast
		}
		out = append(out, frag)
	}
	return out
}
