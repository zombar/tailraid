// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
)

// NTLM is what SMB2 authenticates with when there is no Kerberos, which is
// every case this server is for: a disk image served on a laptop has no domain
// controller behind it. Only NTLMv2 is accepted -- NTLMv1's response is a DES
// construction that a modern client will not send and that nothing should
// accept in 2026.
//
// The messages are MS-NLMP's three: the client says what it can do (type 1),
// the server sends a challenge (type 2), the client answers it (type 3).

var ntlmSignature = [8]byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}

const (
	ntlmNegotiate uint32 = 1
	ntlmChallenge uint32 = 2
	ntlmAuth      uint32 = 3
)

// Negotiate flags. Only the ones this server asserts or reads are named.
const (
	ntlmNegotiateUnicode     uint32 = 0x00000001
	ntlmRequestTarget        uint32 = 0x00000004
	ntlmNegotiateSign        uint32 = 0x00000010
	ntlmNegotiateSeal        uint32 = 0x00000020
	ntlmNegotiateNTLM        uint32 = 0x00000200
	ntlmNegotiateAlwaysSign  uint32 = 0x00008000
	ntlmTargetTypeServer     uint32 = 0x00020000
	ntlmNegotiateExtSecurity uint32 = 0x00080000
	ntlmNegotiateTargetInfo  uint32 = 0x00800000
	ntlmNegotiateVersion     uint32 = 0x02000000
	ntlmNegotiate128         uint32 = 0x20000000
	ntlmNegotiateKeyExchange uint32 = 0x40000000
	ntlmNegotiate56          uint32 = 0x80000000
)

// AV pair ids in the target info a challenge carries.
const (
	avEOL             uint16 = 0x0000
	avNetBIOSDomain   uint16 = 0x0002
	avNetBIOSComputer uint16 = 0x0001
	avDNSComputer     uint16 = 0x0003
	avDNSDomain       uint16 = 0x0004
	avTimestamp       uint16 = 0x0007
)

var errNotNTLM = errors.New("smb: not an NTLM message")

// challenge is what the server remembers between the second and third message.
type challenge struct {
	nonce      [8]byte
	targetInfo []byte
	flags      uint32
}

// newChallenge builds the type 2 message. The target info matters more than it
// looks: an NTLMv2 response is computed OVER it, so a client that is sent one
// signs exactly these bytes back, and a server that invents different ones at
// verification time will reject every correct password.
func newChallenge(serverName string) (*challenge, []byte, error) {
	c := &challenge{
		flags: ntlmNegotiateUnicode | ntlmRequestTarget | ntlmNegotiateNTLM |
			ntlmNegotiateAlwaysSign | ntlmTargetTypeServer | ntlmNegotiateExtSecurity |
			ntlmNegotiateTargetInfo | ntlmNegotiate128 | ntlmNegotiate56 |
			ntlmNegotiateSign | ntlmNegotiateKeyExchange,
	}
	if _, err := rand.Read(c.nonce[:]); err != nil {
		return nil, nil, err
	}
	name := utf16le(serverName)
	var info bytes.Buffer
	putAV(&info, avNetBIOSDomain, name)
	putAV(&info, avNetBIOSComputer, name)
	putAV(&info, avDNSDomain, name)
	putAV(&info, avDNSComputer, name)
	putAV(&info, avTimestamp, filetimeBytes(time.Now()))
	putAV(&info, avEOL, nil)
	c.targetInfo = info.Bytes()

	// Layout: 48 bytes of fixed fields, then the two variable payloads.
	msg := make([]byte, 48)
	copy(msg, ntlmSignature[:])
	binary.LittleEndian.PutUint32(msg[8:], ntlmChallenge)
	targetOff := uint32(48)
	infoOff := targetOff + uint32(len(name))
	putField(msg[12:], len(name), targetOff)
	binary.LittleEndian.PutUint32(msg[20:], c.flags)
	copy(msg[24:], c.nonce[:])
	putField(msg[40:], len(c.targetInfo), infoOff)
	msg = append(msg, name...)
	msg = append(msg, c.targetInfo...)
	return c, msg, nil
}

// authMessage is the type 3 message, as much of it as this server reads.
type authMessage struct {
	domain        string
	user          string
	workstation   string
	ntResponse    []byte
	lmResponse    []byte
	sessionKeyEnc []byte
	flags         uint32
}

func parseAuth(b []byte) (*authMessage, error) {
	if len(b) < 64 || !bytes.HasPrefix(b, ntlmSignature[:]) {
		return nil, errNotNTLM
	}
	if binary.LittleEndian.Uint32(b[8:]) != ntlmAuth {
		return nil, fmt.Errorf("%w: expected the third message", errNotNTLM)
	}
	field := func(off int) ([]byte, error) {
		n := int(binary.LittleEndian.Uint16(b[off:]))
		start := int(binary.LittleEndian.Uint32(b[off+4:]))
		if n == 0 {
			return nil, nil
		}
		if start < 0 || start+n > len(b) {
			return nil, fmt.Errorf("%w: a field runs past the end of the message", errNotNTLM)
		}
		return b[start : start+n], nil
	}
	var (
		a   authMessage
		err error
	)
	if a.lmResponse, err = field(12); err != nil {
		return nil, err
	}
	if a.ntResponse, err = field(20); err != nil {
		return nil, err
	}
	domain, err := field(28)
	if err != nil {
		return nil, err
	}
	user, err := field(36)
	if err != nil {
		return nil, err
	}
	ws, err := field(44)
	if err != nil {
		return nil, err
	}
	if a.sessionKeyEnc, err = field(52); err != nil {
		return nil, err
	}
	a.flags = binary.LittleEndian.Uint32(b[60:])
	a.domain, a.user, a.workstation = fromUTF16le(domain), fromUTF16le(user), fromUTF16le(ws)
	return &a, nil
}

// verify checks the client's NTLMv2 response against a password, and returns
// the session base key on success.
//
// The computation is MS-NLMP 3.3.2, and every part of it is load-bearing: the
// key is HMAC-MD5 over the UPPERCASED user name and the domain AS THE CLIENT
// SENT IT, so a server that upper-cases the domain too, or that substitutes
// its own, computes a different key and rejects a correct password.
func (c *challenge) verify(a *authMessage, cred credential) ([]byte, bool) {
	if len(a.ntResponse) < 16 {
		return nil, false
	}
	key := ntowfv2(a.user, a.domain, cred)
	proof := a.ntResponse[:16]
	blob := a.ntResponse[16:]
	mac := hmac.New(md5.New, key)
	mac.Write(c.nonce[:])
	mac.Write(blob)
	if !hmac.Equal(proof, mac.Sum(nil)) {
		return nil, false
	}
	// The session base key is HMAC-MD5 of the proof under the same key.
	base := hmac.New(md5.New, key)
	base.Write(proof)
	sessionKey := base.Sum(nil)

	// KEY EXCHANGE. When the client asks for it -- macOS and Windows both do
	// -- it invents the session key itself, encrypts it with RC4 under the
	// base key, and sends it along. A server that signs with the base key
	// instead signs with a key the client is not using, and every signature
	// it produces is rejected.
	if a.flags&ntlmNegotiateKeyExchange != 0 && len(a.sessionKeyEnc) == 16 {
		c, err := rc4.NewCipher(sessionKey)
		if err != nil {
			return nil, false
		}
		exported := make([]byte, 16)
		c.XORKeyStream(exported, a.sessionKeyEnc)
		sessionKey = exported
	}
	return sessionKey, true
}

// ntowfv2 is HMAC_MD5(MD4(UTF16LE(password)), UTF16LE(upper(user) + domain)).
//
// The key is the MD4, which is why a server that holds only THAT -- from an
// LDAP directory, say -- can authenticate somebody just as well as one holding
// the password. See [Server.AddUserHash].
func ntowfv2(user, domain string, cred credential) []byte {
	mac := hmac.New(md5.New, cred.key())
	mac.Write(utf16le(upperASCII(user)))
	mac.Write(utf16le(domain))
	return mac.Sum(nil)
}

// upperASCII upper-cases the way NTLM does: byte by byte, without a locale.
// strings.ToUpper would map the Turkish dotless i to something a Windows
// client did not, and the resulting key would not match.
func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func putField(b []byte, n int, off uint32) {
	binary.LittleEndian.PutUint16(b, uint16(n))
	binary.LittleEndian.PutUint16(b[2:], uint16(n))
	binary.LittleEndian.PutUint32(b[4:], off)
}

func putAV(w *bytes.Buffer, id uint16, v []byte) {
	binary.Write(w, binary.LittleEndian, id)
	binary.Write(w, binary.LittleEndian, uint16(len(v)))
	w.Write(v)
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[i*2:], r)
	}
	return b
}

func fromUTF16le(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

func filetimeBytes(t time.Time) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, filetime(t))
	return b
}

// filetime is Windows' clock: 100-nanosecond ticks since 1601.
func filetime(t time.Time) uint64 {
	const epochDelta = 11644473600 // seconds from 1601 to 1970
	return uint64(t.UTC().Unix()+epochDelta)*10_000_000 + uint64(t.Nanosecond()/100)
}
