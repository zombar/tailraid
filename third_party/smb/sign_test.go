package smb

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The two algorithms, chosen by dialect, and the derivation that goes with
// each: 2.x keys HMAC-SHA256 with the session key itself, 3.x keys AES-CMAC
// with a key derived from it.
func TestSigningKeyDependsOnTheDialect(t *testing.T) {
	sessionKey := []byte("0123456789abcdef")
	for _, d := range []uint16{dialect202, dialect210} {
		if got := signingKeyFor(d, sessionKey); !bytes.Equal(got, sessionKey) {
			t.Errorf("dialect %#04x derives a key where it should use the session key", d)
		}
	}
	for _, d := range []uint16{dialect300, dialect302} {
		got := signingKeyFor(d, sessionKey)
		if bytes.Equal(got, sessionKey) {
			t.Errorf("dialect %#04x signs with the session key itself", d)
		}
		if len(got) != 16 {
			t.Errorf("dialect %#04x derived %d bytes", d, len(got))
		}
	}
	// The derivation is a function of its inputs, and a different label gives
	// a different key -- which is what stops a signing key doubling as an
	// encryption key.
	if bytes.Equal(kdf(sessionKey, "SMB2AESCMAC\x00", "SmbSign\x00"),
		kdf(sessionKey, "SMB2AESCCM\x00", "ServerIn \x00")) {
		t.Error("two labels derived the same key")
	}
}

// A signature covers the whole message with its own field zeroed, so signing
// and verifying are the same computation from the two sides.
func TestSignAndVerifyRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef")
	for _, d := range []uint16{dialect210, dialect300} {
		msg := append(responseTo(header{command: cmdEcho, messageID: 3}, statusSuccess), 1, 2, 3, 4)
		signMessage(d, key, msg)
		if binary.LittleEndian.Uint32(msg[offFlags:])&flagSigned == 0 {
			t.Errorf("dialect %#04x signed without saying so", d)
		}
		if !verifyMessage(d, key, msg) {
			t.Errorf("dialect %#04x did not verify its own signature", d)
		}
		// …and verifying must not have changed the message, or the next
		// reader sees something else.
		if !verifyMessage(d, key, msg) {
			t.Errorf("dialect %#04x verified once and then failed", d)
		}
		// One byte anywhere breaks it.
		for _, at := range []int{offCommand, headerLen, len(msg) - 1} {
			bad := append([]byte(nil), msg...)
			bad[at] ^= 0xFF
			if verifyMessage(d, key, bad) {
				t.Errorf("dialect %#04x accepted a message changed at byte %d", d, at)
			}
		}
		// Another key does not.
		if verifyMessage(d, []byte("fedcba9876543210"), msg) {
			t.Errorf("dialect %#04x accepted a signature under another key", d)
		}
	}
	// Nothing to sign with, or nothing to sign: no signature and no panic.
	msg := responseTo(header{command: cmdEcho}, statusSuccess)
	signMessage(dialect210, nil, msg)
	if binary.LittleEndian.Uint32(msg[offFlags:])&flagSigned != 0 {
		t.Error("a message was marked signed with no key")
	}
	if verifyMessage(dialect210, nil, msg) || verifyMessage(dialect210, key, msg[:10]) {
		t.Error("something without a key or a header verified")
	}
}

// The validation exchange exists because the dialect negotiation happens
// before there is a session to sign it: the client asks the server to repeat
// itself, signed, and compares.
func TestValidateNegotiateInfo(t *testing.T) {
	c := &conn{srv: New(), dialect: dialect302, clientSecurityMode: signingEnabled, clientCapabilities: 0x7f}

	ask := func(caps uint32, mode uint16) []byte {
		in := make([]byte, 24)
		binary.LittleEndian.PutUint32(in[0:], caps)
		binary.LittleEndian.PutUint16(in[20:], mode)
		body := make([]byte, 56)
		binary.LittleEndian.PutUint32(body[4:], fsctlValidateNegotiateInfo)
		binary.LittleEndian.PutUint32(body[24:], uint32(headerLen+56))
		binary.LittleEndian.PutUint32(body[28:], uint32(len(in)))
		out, err := c.dispatch(request(cmdIoctl, 0, append(body, in...)))
		if err != nil {
			t.Fatalf("the control code: %v", err)
		}
		return out
	}

	out := ask(0x7f, signingEnabled)
	if st := statusOf(t, out); st != statusSuccess {
		t.Fatalf("a matching validation answered %#x", st)
	}
	// The client reads the payload through the OUTPUT offset and count, and a
	// response that fills the input ones instead looks empty to it.
	if got := binary.LittleEndian.Uint32(out[headerLen+36:]); got != 24 {
		t.Errorf("the response says its output is %d bytes, want 24", got)
	}
	if got := binary.LittleEndian.Uint32(out[headerLen+32:]); got != headerLen+48 {
		t.Errorf("the response says its output starts at %d", got)
	}
	// What comes back must be what was negotiated, or the client concludes it
	// was tampered with.
	res := out[headerLen+48:]
	if got := binary.LittleEndian.Uint16(res[22:]); got != dialect302 {
		t.Errorf("the validation says the dialect is %#04x", got)
	}
	if !bytes.Equal(res[4:20], c.srv.guid[:]) {
		t.Error("the validation reports a different server identity than the negotiate did")
	}

	// A client whose memory of the exchange differs is told so.
	if st := statusOf(t, ask(0x01, signingEnabled)); st != statusAccessDenied {
		t.Errorf("mismatched capabilities answered %#x", st)
	}
	if st := statusOf(t, ask(0x7f, signingRequired)); st != statusAccessDenied {
		t.Errorf("a mismatched security mode answered %#x", st)
	}

	// Everything else is refused by name.
	body := make([]byte, 56)
	binary.LittleEndian.PutUint32(body[4:], fsctlDfsGetReferrals)
	if st := statusOf(t, mustDispatch(t, c, cmdIoctl, body)); st != statusNotSupported {
		t.Error("a referral request was not refused by name")
	}
	binary.LittleEndian.PutUint32(body[4:], 0x00090000)
	if st := statusOf(t, mustDispatch(t, c, cmdIoctl, body)); st != statusNotSupported {
		t.Error("an unknown control code was not refused by name")
	}
	// A payload that is not inside the message is refused rather than read.
	binary.LittleEndian.PutUint32(body[4:], fsctlValidateNegotiateInfo)
	binary.LittleEndian.PutUint32(body[24:], 1<<20)
	binary.LittleEndian.PutUint32(body[28:], 24)
	if st := statusOf(t, mustDispatch(t, c, cmdIoctl, body)); st != statusInvalidParameter {
		t.Error("a payload outside the message was read anyway")
	}
	if _, err := c.dispatch(request(cmdIoctl, 0, make([]byte, 4))); err == nil {
		t.Error("a body too short to read was accepted")
	}
}
