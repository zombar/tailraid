package smb

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// The framing is four bytes in front of every message, and three of them are a
// length. Both edges of that are refusals rather than surprises.
func TestFraming(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes()[:4]; !bytes.Equal(got, []byte{0, 0, 0, 5}) {
		t.Errorf("header = %v, want a zero type and a length of 5", got)
	}
	body, err := readFrame(&buf)
	if err != nil || string(body) != "hello" {
		t.Errorf("readFrame = %q, %v", body, err)
	}

	for _, tc := range []struct {
		name string
		in   []byte
		want string
	}{
		{"a message type that is not a session message", []byte{0x81, 0, 0, 4, 1, 2, 3, 4}, "not a session message"},
		{"an empty message", []byte{0, 0, 0, 0}, "empty"},
		{"a message larger than the limit", []byte{0, 0xFF, 0xFF, 0xFF}, "larger than"},
		{"a truncated header", []byte{0, 0}, "unexpected EOF"},
		{"a body that stops early", []byte{0, 0, 0, 8, 1, 2}, "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readFrame(bytes.NewReader(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if err := writeFrame(&bytes.Buffer{}, make([]byte, maxFrameLen)); err == nil {
		t.Error("writeFrame accepted a message that does not fit a three-byte length")
	}
}

// A header this server cannot read is an error with a reason, because the next
// thing it would do is read fields out of the wrong offsets.
func TestHeaderRefusals(t *testing.T) {
	good := responseTo(header{command: cmdEcho, messageID: 7}, statusSuccess)
	h, err := parseHeader(good)
	if err != nil {
		t.Fatal(err)
	}
	if h.command != cmdEcho || h.messageID != 7 || h.flags&flagServerToRedir == 0 {
		t.Errorf("round trip lost something: %+v", h)
	}
	for _, tc := range []struct {
		name string
		fix  func([]byte)
		want string
	}{
		{"a protocol id that is not SMB2", func(b []byte) { b[0] = 0xFF }, "protocol id"},
		{"a header that says it is another size", func(b []byte) { binary.LittleEndian.PutUint16(b[offStructSize:], 32) }, "not 64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			tc.fix(b)
			if _, err := parseHeader(b); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := parseHeader([]byte{1, 2}); err == nil {
		t.Error("parseHeader accepted something shorter than a header")
	}
	if got := cmdCreate.String(); got != "CREATE" {
		t.Errorf("cmdCreate.String() = %q", got)
	}
	if got := command(0x4242).String(); !strings.Contains(got, "4242") {
		t.Errorf("an unknown command prints as %q", got)
	}
}

// The SPNEGO envelope: what the server emits must be what its own reader
// accepts, and every shape a client actually sends must be read.
func TestSPNEGO(t *testing.T) {
	ntlm := append(ntlmSignature[:], []byte{1, 0, 0, 0, 0, 0, 0, 0}...)

	// A NegTokenInit, as a client sends first: the SPNEGO OID, a mech list,
	// and the token.
	init := derApp(0, append(derOID(oidSPNEGO),
		derCtx(0, derSeq(append(derCtx(0, derSeq(derOID(oidNTLMSSP))), derCtx(2, derOctet(ntlm))...)))...))
	got, wrapped, err := mechTokenOf(init)
	if err != nil || !bytes.Equal(got, ntlm) || !wrapped {
		t.Errorf("NegTokenInit: %v, %x, wrapped=%v", err, got, wrapped)
	}

	// A NegTokenResp, as it sends second.
	resp := derCtx(1, derSeq(derCtx(2, derOctet(ntlm))))
	got, wrapped, err = mechTokenOf(resp)
	if err != nil || !bytes.Equal(got, ntlm) || !wrapped {
		t.Errorf("NegTokenResp: %v, %x, wrapped=%v", err, got, wrapped)
	}

	// And a bare NTLM message with no envelope at all.
	// A bare message, which is what the Linux kernel's client sends -- and the
	// answer has to be bare too.
	if got, wrapped, err = mechTokenOf(ntlm); err != nil || !bytes.Equal(got, ntlm) || wrapped {
		t.Errorf("bare NTLMSSP: %v, %x, wrapped=%v", err, got, wrapped)
	}

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"nothing", nil},
		{"a tag this server does not read", []byte{0x31, 0x00}},
		{"a length that runs past the end", []byte{0x60, 0x40, 0x06}},
		{"a mechanism that is not SPNEGO", derApp(0, derOID(oidNTLMSSP))},
		{"an envelope with no token in it", derCtx(1, derSeq(derCtx(0, derEnum(0))))},
		{"a length nobody encodes that way", []byte{0x60, 0x85, 1, 2, 3, 4, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := mechTokenOf(tc.in); !errors.Is(err, errNotSPNEGO) {
				t.Errorf("error = %v, want errNotSPNEGO", err)
			}
		})
	}

	// The token the server offers must name NTLM, and its own reader must
	// agree that it is a token.
	offered := negTokenInitNTLM()
	if !bytes.Contains(offered, oidNTLMSSP) {
		t.Error("the offered token does not name NTLM")
	}
	// A long token exercises the two-byte length form on the way out.
	long := negTokenRespChallenge(make([]byte, 300))
	if tok, _, err := mechTokenOf(long); err != nil || len(tok) != 300 {
		t.Errorf("a 300-byte challenge came back as %d bytes, %v", len(tok), err)
	}
	if tok, _, err := mechTokenOf(negTokenRespChallenge(make([]byte, 200))); err != nil || len(tok) != 200 {
		t.Errorf("a 200-byte challenge came back as %d bytes, %v", len(tok), err)
	}
}

// NTLM, without a client: a challenge this server made, answered the way the
// specification says, is accepted; the same answer under another password is
// not.
func TestNTLMv2(t *testing.T) {
	ch, msg, err := newChallenge("SERVER")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(msg, ntlmSignature[:]) || binary.LittleEndian.Uint32(msg[8:]) != ntlmChallenge {
		t.Fatal("the challenge is not an NTLM type 2 message")
	}
	auth := clientAuth(t, ch, "alice", "WORKGROUP", "hunter2")
	key, ok := ch.verify(auth, credential{password: "hunter2"})
	if !ok {
		t.Fatal("the server refused a correct answer")
	}
	if len(key) != 16 {
		t.Errorf("the session key is %d bytes, want 16", len(key))
	}
	if _, ok := ch.verify(auth, credential{password: "not it"}); ok {
		t.Error("the server accepted an answer under the wrong password")
	}
	// The user name is compared upper-cased and the domain is not: an answer
	// computed with a lower-case user name is the same answer.
	lower := clientAuth(t, ch, "ALICE", "WORKGROUP", "hunter2")
	if _, ok := ch.verify(lower, credential{password: "hunter2"}); !ok {
		t.Error("the case of the user name changed the answer")
	}
	if _, ok := ch.verify(&authMessage{user: "alice", ntResponse: []byte{1, 2, 3}}, credential{password: "hunter2"}); ok {
		t.Error("a response too short to be one was accepted")
	}
	if _, err := parseAuth([]byte("nope")); !errors.Is(err, errNotNTLM) {
		t.Error("parseAuth accepted something that is not an NTLM message")
	}
	if _, err := parseAuth(msg); !errors.Is(err, errNotNTLM) {
		t.Error("parseAuth accepted a challenge as if it were an answer")
	}
	if got := upperASCII("mixedCASE-123"); got != "MIXEDCASE-123" {
		t.Errorf("upperASCII = %q", got)
	}
	if got := fromUTF16le([]byte{1}); got != "" {
		t.Errorf("an odd number of bytes decoded to %q", got)
	}
}

// clientAuth builds the answer a client sends, so the server's half can be
// exercised without one. It uses this package's own primitives, so it proves
// the PARSING and the branching rather than the formula -- the independent
// check on the formula is the foreign client in handshake_test.go, and the
// macOS client recorded in the README.
func clientAuth(t *testing.T, ch *challenge, user, domain, password string) *authMessage {
	t.Helper()
	blob := []byte{0x01, 0x01, 0, 0, 0, 0, 0, 0}
	blob = append(blob, filetimeBytes(time.Now())...)
	blob = append(blob, 1, 2, 3, 4, 5, 6, 7, 8) // the client's own challenge
	blob = append(blob, 0, 0, 0, 0)
	blob = append(blob, ch.targetInfo...)
	blob = append(blob, 0, 0, 0, 0)

	mac := hmac.New(md5.New, ntowfv2(user, domain, credential{password: password}))
	mac.Write(ch.nonce[:])
	mac.Write(blob)
	nt := append(mac.Sum(nil), blob...)

	// Serialise it, then parse it back, so the test goes through the same
	// reader the wire does.
	msg := make([]byte, 64)
	copy(msg, ntlmSignature[:])
	binary.LittleEndian.PutUint32(msg[8:], ntlmAuth)
	u, d := utf16le(user), utf16le(domain)
	off := uint32(64)
	putField(msg[20:], len(nt), off) // NT response
	off += uint32(len(nt))
	putField(msg[28:], len(d), off) // domain
	off += uint32(len(d))
	putField(msg[36:], len(u), off) // user
	msg = append(msg, nt...)
	msg = append(msg, d...)
	msg = append(msg, u...)
	got, err := parseAuth(msg)
	if err != nil {
		t.Fatalf("the message this test built does not parse: %v", err)
	}
	return got
}
