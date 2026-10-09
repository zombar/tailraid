// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// Signing is what makes a message provably from the party that authenticated,
// and it is not optional in practice: Windows 11 requires it of every
// connection by default, so a server that cannot sign cannot be mounted by the
// client this package exists for.
//
// Two algorithms, chosen by dialect:
//
//	2.0.2, 2.1        HMAC-SHA256, keyed with the session key itself
//	3.0, 3.0.2        AES-CMAC, keyed with a key DERIVED from it
//
// The signature covers the whole message with its own signature field zeroed,
// which is why signing is the last thing done to a reply and verification the
// first thing done to a request.

// signingKeyFor derives the key a dialect signs with.
func signingKeyFor(dialect uint16, sessionKey []byte) []byte {
	switch dialect {
	case dialect300, dialect302:
		return kdf(sessionKey, "SMB2AESCMAC\x00", "SmbSign\x00")
	default:
		return sessionKey
	}
}

// kdf is NIST SP 800-108 in counter mode over HMAC-SHA256, which is how SMB 3
// derives every key it uses from the session key. One iteration is enough: the
// output is 128 bits and the PRF gives 256.
func kdf(key []byte, label, context string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte{0x00, 0x00, 0x00, 0x01}) // i
	h.Write([]byte(label))                  // includes its terminating zero
	h.Write([]byte{0x00})                   // the separator
	h.Write([]byte(context))                // likewise
	h.Write([]byte{0x00, 0x00, 0x00, 0x80}) // L, in bits
	return h.Sum(nil)[:16]
}

// signatureOf computes the signature a message should carry. The caller has
// already zeroed the field, or is about to compare against what is in it.
func signatureOf(dialect uint16, key, msg []byte) []byte {
	switch dialect {
	case dialect300, dialect302:
		return cmacSum(key, msg)
	default:
		mac := hmac.New(sha256.New, key)
		mac.Write(msg)
		return mac.Sum(nil)[:16]
	}
}

// signMessage writes the signature into a reply, and says so in its flags.
func signMessage(dialect uint16, key, msg []byte) {
	if len(msg) < headerLen || len(key) == 0 {
		return
	}
	flags := binary.LittleEndian.Uint32(msg[offFlags:]) | flagSigned
	binary.LittleEndian.PutUint32(msg[offFlags:], flags)
	clear(msg[offSignature : offSignature+16])
	copy(msg[offSignature:], signatureOf(dialect, key, msg))
}

// verifyMessage checks the signature a request carries.
//
// The message is copied before its signature field is zeroed, because the
// caller still has to read the request afterwards -- and a verifier that
// scribbles on its input is a verifier that changes what it verified.
func verifyMessage(dialect uint16, key, msg []byte) bool {
	if len(msg) < headerLen || len(key) == 0 {
		return false
	}
	want := append([]byte(nil), msg[offSignature:offSignature+16]...)
	scratch := append([]byte(nil), msg...)
	clear(scratch[offSignature : offSignature+16])
	return hmac.Equal(want, signatureOf(dialect, key, scratch))
}
