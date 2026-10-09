// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
)

// The SMB2 header is 64 bytes in front of every message, request and response
// alike. Offsets are named rather than counted at each use, because a header
// field read one field over is the kind of mistake that produces a plausible
// wrong answer instead of an error.
const (
	headerLen = 64

	offProtocolID   = 0 // \xfeSMB
	offStructSize   = 4 // always 64
	offCreditCharge = 6
	offStatus       = 8
	offCommand      = 12
	offCredits      = 14
	offFlags        = 16
	offNextCommand  = 20
	offMessageID    = 24
	offTreeID       = 36
	offSessionID    = 40
	offSignature    = 48
)

var protocolID = [4]byte{0xFE, 'S', 'M', 'B'}

// smb1ProtocolID is the only thing this server ever recognises from SMB1: the
// four bytes in front of the legacy greeting.
var smb1ProtocolID = [4]byte{0xFF, 'S', 'M', 'B'}

// Commands. The gaps are commands this server does not implement; they are
// listed so a log line can name what was asked for rather than print a number.
type command uint16

const (
	cmdNegotiate      command = 0x0000
	cmdSessionSetup   command = 0x0001
	cmdLogoff         command = 0x0002
	cmdTreeConnect    command = 0x0003
	cmdTreeDisconnect command = 0x0004
	cmdCreate         command = 0x0005
	cmdClose          command = 0x0006
	cmdFlush          command = 0x0007
	cmdRead           command = 0x0008
	cmdWrite          command = 0x0009
	cmdLock           command = 0x000A
	cmdIoctl          command = 0x000B
	cmdCancel         command = 0x000C
	cmdEcho           command = 0x000D
	cmdQueryDirectory command = 0x000E
	cmdChangeNotify   command = 0x000F
	cmdQueryInfo      command = 0x0010
	cmdSetInfo        command = 0x0011
	cmdOplockBreak    command = 0x0012
)

var commandNames = map[command]string{
	cmdNegotiate: "NEGOTIATE", cmdSessionSetup: "SESSION_SETUP", cmdLogoff: "LOGOFF",
	cmdTreeConnect: "TREE_CONNECT", cmdTreeDisconnect: "TREE_DISCONNECT",
	cmdCreate: "CREATE", cmdClose: "CLOSE", cmdFlush: "FLUSH", cmdRead: "READ",
	cmdWrite: "WRITE", cmdLock: "LOCK", cmdIoctl: "IOCTL", cmdCancel: "CANCEL",
	cmdEcho: "ECHO", cmdQueryDirectory: "QUERY_DIRECTORY", cmdChangeNotify: "CHANGE_NOTIFY",
	cmdQueryInfo: "QUERY_INFO", cmdSetInfo: "SET_INFO", cmdOplockBreak: "OPLOCK_BREAK",
}

func (c command) String() string {
	if n, ok := commandNames[c]; ok {
		return n
	}
	return fmt.Sprintf("command(%#04x)", uint16(c))
}

// Header flags.
const (
	flagServerToRedir = 0x00000001
	flagAsyncCommand  = 0x00000002
	flagRelatedOps    = 0x00000004
	flagSigned        = 0x00000008
)

// Dialects. 0x02FF is the wildcard a server answers the legacy greeting with:
// it means "send me a real SMB2 negotiate".
const (
	dialectWildcard uint16 = 0x02FF
	dialect202      uint16 = 0x0202
	dialect210      uint16 = 0x0210
	dialect300      uint16 = 0x0300
	dialect302      uint16 = 0x0302
	dialect311      uint16 = 0x0311
)

// Security modes, in both the negotiate and the session setup.
const (
	signingEnabled  uint16 = 0x0001
	signingRequired uint16 = 0x0002
)

// A header is one parsed SMB2 header. Signature is kept as the raw slice into
// the message so signing can zero it and hash in place.
type header struct {
	creditCharge uint16
	status       uint32
	command      command
	credits      uint16
	flags        uint32
	nextCommand  uint32
	messageID    uint64
	treeID       uint32
	sessionID    uint64
	// asyncID is the same eight bytes as treeID and the four before it: a
	// header is one shape or the other, and the flags say which.
	asyncID uint64
}

// parseHeader reads the 64 bytes in front of a message.
func parseHeader(b []byte) (header, error) {
	var h header
	if len(b) < headerLen {
		return h, fmt.Errorf("smb: message of %d bytes is shorter than a header", len(b))
	}
	if [4]byte(b[offProtocolID:offProtocolID+4]) != protocolID {
		return h, fmt.Errorf("smb: message does not begin with the SMB2 protocol id")
	}
	if got := binary.LittleEndian.Uint16(b[offStructSize:]); got != headerLen {
		return h, fmt.Errorf("smb: header says it is %d bytes, not %d", got, headerLen)
	}
	h.creditCharge = binary.LittleEndian.Uint16(b[offCreditCharge:])
	h.status = binary.LittleEndian.Uint32(b[offStatus:])
	h.command = command(binary.LittleEndian.Uint16(b[offCommand:]))
	h.credits = binary.LittleEndian.Uint16(b[offCredits:])
	h.flags = binary.LittleEndian.Uint32(b[offFlags:])
	h.nextCommand = binary.LittleEndian.Uint32(b[offNextCommand:])
	h.messageID = binary.LittleEndian.Uint64(b[offMessageID:])
	h.treeID = binary.LittleEndian.Uint32(b[offTreeID:])
	h.asyncID = binary.LittleEndian.Uint64(b[offAsyncID:])
	h.sessionID = binary.LittleEndian.Uint64(b[offSessionID:])
	return h, nil
}

// responseTo builds the 64-byte header of a reply, echoing the fields a client
// matches replies by.
func responseTo(req header, status uint32) []byte {
	b := make([]byte, headerLen)
	copy(b[offProtocolID:], protocolID[:])
	binary.LittleEndian.PutUint16(b[offStructSize:], headerLen)
	binary.LittleEndian.PutUint16(b[offCreditCharge:], 1)
	binary.LittleEndian.PutUint32(b[offStatus:], status)
	binary.LittleEndian.PutUint16(b[offCommand:], uint16(req.command))
	binary.LittleEndian.PutUint16(b[offCredits:], creditsFor(req))
	binary.LittleEndian.PutUint32(b[offFlags:], flagServerToRedir)
	binary.LittleEndian.PutUint64(b[offMessageID:], req.messageID)
	binary.LittleEndian.PutUint32(b[offTreeID:], req.treeID)
	binary.LittleEndian.PutUint64(b[offSessionID:], req.sessionID)
	return b
}

// creditsFor grants what the client asked for, within a bound.
//
// Credits are not a formality. A client spends one per 64 KiB of a read or a
// write, so a server that always grants one can never be asked for more than
// 64 KiB -- and a client that wants a megabyte simply WAITS, having nothing to
// spend, with no error on either side. macOS asks for 256 up front, and a
// mount that hangs after TREE_CONNECT with nothing in the log is what a
// hardcoded one looks like from the outside.
//
// The cap is what stops a client asking for an unbounded number of
// outstanding operations; granting at least one is what stops the connection
// wedging when it asks for none.
func creditsFor(req header) uint16 {
	const most = 512
	want := req.credits
	if want == 0 {
		want = 1
	}
	if want > most {
		want = most
	}
	return want
}
