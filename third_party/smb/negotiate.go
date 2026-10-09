// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
	"time"
)

// The sizes this server advertises. They bound one message, and a client will
// not ask for more than it was told.
const (
	maxTransactSize = 1 << 20
	maxReadSize     = 1 << 20
	maxWriteSize    = 1 << 20
)

// dialectsWeSpeak, in the order a server should prefer.
//
// 3.0.2 and 3.0 sign with AES-CMAC and derive their key rather than using the
// session key directly; 2.1 signs with HMAC-SHA256. All three are honoured
// here. 3.1.1 is not: it adds pre-authentication integrity and negotiate
// contexts, which change the shape of the exchange itself, and naming it
// without implementing them would be promising what is not there.
//
// 2.0.2 exists only for Vista and is left out.
var dialectsWeSpeak = []uint16{dialect302, dialect300, dialect210}

// legacyNegotiateResponse answers the one SMB1 message this server reads.
//
// A client that has spoken SMB2 before still opens with the 1996 greeting,
// offering "NT LM 0.12", "SMB 2.002" and "SMB 2.???" -- measured, on macOS 26.
// The answer is an SMB2 NEGOTIATE response naming the wildcard dialect 0x02FF,
// which means "send me a real SMB2 negotiate". The contents of the SMB1
// message are never parsed: there is exactly one thing to say back.
func (c *conn) legacyNegotiateResponse() ([]byte, error) {
	return c.negotiateResponse(header{command: cmdNegotiate}, dialectWildcard), nil
}

// negotiate answers an SMB2 NEGOTIATE by choosing a dialect both sides speak.
func (c *conn) negotiate(h header, body []byte) ([]byte, error) {
	if len(body) < 36 {
		return nil, fmt.Errorf("smb: NEGOTIATE body of %d bytes is too short", len(body))
	}
	count := int(binary.LittleEndian.Uint16(body[2:]))
	offered := make([]uint16, 0, count)
	for i, off := 0, 36; i < count && off+2 <= len(body); i, off = i+1, off+2 {
		offered = append(offered, binary.LittleEndian.Uint16(body[off:]))
	}
	chosen := uint16(0)
	for _, ours := range dialectsWeSpeak {
		for _, theirs := range offered {
			if ours == theirs {
				chosen = ours
				break
			}
		}
		if chosen != 0 {
			break
		}
	}
	if chosen == 0 {
		// NOT_SUPPORTED rather than a dialect nobody asked for: a client told
		// "0x0210" when it never offered it has no way to interpret the rest.
		return errorResponse(h, statusNotSupported), nil
	}
	c.dialect = chosen
	c.clientSecurityMode = binary.LittleEndian.Uint16(body[4:])
	c.clientCapabilities = binary.LittleEndian.Uint32(body[8:])
	return c.negotiateResponse(h, chosen), nil
}

// negotiateResponse builds the header and body of a NEGOTIATE response.
func (c *conn) negotiateResponse(h header, dialect uint16) []byte {
	// The security buffer says which authentication mechanisms exist. Offering
	// exactly one, NTLM, is what makes the client's next message predictable.
	sec := negTokenInitNTLM()

	const bodyLen = 64 // the fixed part; structure size says 65 for "and a buffer"
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+len(sec))...)
	body := b[headerLen:]
	binary.LittleEndian.PutUint16(body[0:], 65)
	binary.LittleEndian.PutUint16(body[2:], signingEnabled)
	binary.LittleEndian.PutUint16(body[4:], dialect)
	binary.LittleEndian.PutUint32(body[24:], capabilitiesFor(dialect))
	copy(body[8:24], c.srv.guid[:]) // identity, not a secret -- and stable, because
	//                                 the validate-negotiate exchange below
	//                                 compares it against what was sent here
	binary.LittleEndian.PutUint32(body[28:], maxTransactSize)
	binary.LittleEndian.PutUint32(body[32:], maxReadSize)
	binary.LittleEndian.PutUint32(body[36:], maxWriteSize)
	now := filetime(time.Now())
	binary.LittleEndian.PutUint64(body[40:], now)
	binary.LittleEndian.PutUint64(body[48:], now)
	binary.LittleEndian.PutUint16(body[56:], headerLen+bodyLen)
	binary.LittleEndian.PutUint16(body[58:], uint16(len(sec)))
	copy(body[bodyLen:], sec)
	return b
}
