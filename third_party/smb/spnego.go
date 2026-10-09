// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"bytes"
	"errors"
	"fmt"
)

// SPNEGO is the envelope SMB2 puts around NTLM. Only the shape actually seen
// on the wire is handled here -- a NegTokenInit offering one mechanism and a
// NegTokenResp carrying one token -- rather than a general DER library, and
// anything else is refused instead of guessed at.
//
// The alternative was to depend on an ASN.1 package for four fixed shapes.
// This module has no dependency outside the standard library, and these
// shapes have been the same since 2005.

var (
	oidSPNEGO  = []byte{0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}                         // 1.3.6.1.5.5.2
	oidNTLMSSP = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a} // 1.3.6.1.4.1.311.2.2.10
)

// errNotSPNEGO says the blob is not a token this server can read. A caller
// turns it into a logon failure rather than a crash.
var errNotSPNEGO = errors.New("smb: not an SPNEGO token this server understands")

// negTokenInitNTLM is the "I can do NTLM, and nothing else" token the server
// puts in its NEGOTIATE response. Offering exactly one mechanism is what makes
// the client's reply predictable: it echoes the list back and picks the only
// entry.
func negTokenInitNTLM() []byte {
	mechList := derSeq(derOID(oidNTLMSSP))
	init := derCtx(0, derSeq(derCtx(0, mechList)))
	return derApp(0, append(derOID(oidSPNEGO), init...))
}

// negTokenRespChallenge wraps the NTLM challenge the server sends back:
// negState = accept-incomplete (1), and the token itself.
func negTokenRespChallenge(token []byte) []byte {
	return derCtx(1, derSeq(
		append(derCtx(0, derEnum(1)),
			append(derCtx(1, derOID(oidNTLMSSP)), derCtx(2, derOctet(token))...)...)))
}

// negTokenRespAccept is the empty "and we are done" token.
func negTokenRespAccept() []byte {
	return derCtx(1, derSeq(derCtx(0, derEnum(0))))
}

// mechTokenOf digs the NTLM message out of whichever shape the client sent: a
// NegTokenInit (its first message), a NegTokenResp (its second), or no
// envelope at all.
//
// The second return says WHICH, and it is not a detail. The Linux kernel's
// client speaks "raw NTLMSSP" -- it sends a bare message and expects a bare
// one back -- while macOS wraps both in SPNEGO. Answering a bare message with
// a wrapped one gets `CIFS: VFS: blob signature incorrect` in the kernel log
// and mount error(22) at the console: it reads the first eight bytes of the
// reply, looks for "NTLMSSP", and finds ASN.1.
func mechTokenOf(blob []byte) (token []byte, wrapped bool, err error) {
	if bytes.HasPrefix(blob, ntlmSignature[:]) {
		return blob, false, nil
	}
	if len(blob) == 0 {
		return nil, false, errNotSPNEGO
	}
	body := blob
	if body[0] == 0x60 { // [APPLICATION 0]: a NegTokenInit, with the SPNEGO OID in front
		var err error
		if body, err = derUnwrap(body, 0x60); err != nil {
			return nil, false, err
		}
		oid, rest, err := derSplit(body, 0x06)
		if err != nil {
			return nil, false, err
		}
		if !bytes.Equal(oid, oidSPNEGO) {
			return nil, false, fmt.Errorf("%w: the mechanism is not SPNEGO", errNotSPNEGO)
		}
		body = rest
	}
	// Now: [0] NegTokenInit or [1] NegTokenResp, each a SEQUENCE of tagged
	// fields. The one we want is the octet string -- mechToken in the first
	// shape, responseToken in the second.
	inner, err := derUnwrapAny(body, 0xA0, 0xA1)
	if err != nil {
		return nil, false, err
	}
	seq, err := derUnwrap(inner, 0x30)
	if err != nil {
		return nil, false, err
	}
	for len(seq) > 0 {
		tag := seq[0]
		content, rest, err := derSplit(seq, tag)
		if err != nil {
			return nil, false, err
		}
		// mechToken is [2] in a NegTokenInit and responseToken is [2] in a
		// NegTokenResp: the same tag, which is why one loop reads both.
		if tag == 0xA2 {
			tok, err := derUnwrap(content, 0x04)
			return tok, true, err
		}
		seq = rest
	}
	return nil, false, fmt.Errorf("%w: there is no token in it", errNotSPNEGO)
}

// ─── just enough DER ─────────────────────────────────────────────────────────

func derLen(n int) []byte {
	switch {
	case n < 0x80:
		return []byte{byte(n)}
	case n < 0x100:
		return []byte{0x81, byte(n)}
	default:
		return []byte{0x82, byte(n >> 8), byte(n)}
	}
}

func derTLV(tag byte, content []byte) []byte {
	out := append([]byte{tag}, derLen(len(content))...)
	return append(out, content...)
}

func derApp(n byte, content []byte) []byte { return derTLV(0x60|n, content) }
func derCtx(n byte, content []byte) []byte { return derTLV(0xA0|n, content) }
func derSeq(content []byte) []byte         { return derTLV(0x30, content) }
func derOID(oid []byte) []byte             { return derTLV(0x06, oid) }
func derOctet(b []byte) []byte             { return derTLV(0x04, b) }
func derEnum(v byte) []byte                { return derTLV(0x0A, []byte{v}) }

// derSplit reads one TLV with the expected tag off the front, returning its
// content and whatever follows it.
func derSplit(b []byte, tag byte) (content, rest []byte, err error) {
	if len(b) < 2 || b[0] != tag {
		return nil, nil, fmt.Errorf("%w: expected tag %#02x", errNotSPNEGO, tag)
	}
	n, hdr, err := derReadLen(b[1:])
	if err != nil {
		return nil, nil, err
	}
	start := 1 + hdr
	if start+n > len(b) {
		return nil, nil, fmt.Errorf("%w: a length runs past the end", errNotSPNEGO)
	}
	return b[start : start+n], b[start+n:], nil
}

func derUnwrap(b []byte, tag byte) ([]byte, error) {
	content, _, err := derSplit(b, tag)
	return content, err
}

func derUnwrapAny(b []byte, tags ...byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errNotSPNEGO
	}
	for _, t := range tags {
		if b[0] == t {
			return derUnwrap(b, t)
		}
	}
	return nil, fmt.Errorf("%w: tag %#02x is not one this server reads", errNotSPNEGO, b[0])
}

// derReadLen returns the length and how many bytes encoded it.
func derReadLen(b []byte) (n, size int, err error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("%w: a length is missing", errNotSPNEGO)
	}
	if b[0] < 0x80 {
		return int(b[0]), 1, nil
	}
	count := int(b[0] & 0x7F)
	if count == 0 || count > 3 || len(b) < 1+count {
		return 0, 0, fmt.Errorf("%w: a length of %d bytes is not one this server reads", errNotSPNEGO, count)
	}
	for i := 1; i <= count; i++ {
		n = n<<8 | int(b[i])
	}
	return n, 1 + count, nil
}
