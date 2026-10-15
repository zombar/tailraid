package users

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// A browser session (docs/21 §6, I-354) is a 32-byte token whose SHA-256 is the FSM's
// row; the browser holds the token in a cookie. The token reaches a browser through a
// one-time code: the expiry and the token sealed with AES-256-GCM under a key derived
// from the cluster key, so any node opens a code any other node minted (`web login` on
// one node, a hand-off between two nodes' pages) and nothing about codes is replicated,
// while a code copied from a browser's history or a proxy's log yields no token to
// anyone without the cluster key. A node remembers the codes it opened until they
// expire, so each works once there.

const (
	// CodeLife is how long a code is good for.
	CodeLife = 5 * time.Minute
	// CodeLogMax bounds the opened codes a node remembers; the oldest go first. At one
	// sign-in a second it covers far more than CodeLife.
	CodeLogMax = 4096
	// TokenSize is a session token's size.
	TokenSize = 32

	codeNonce = 12 // the GCM nonce; its first CodeNonceLen bytes name the code to the CodeLog
	codeTag   = 16
	codeLen   = codeNonce + 8 + TokenSize + codeTag
	// CodeNonceLen is how much of a code's nonce the CodeLog keys on.
	CodeNonceLen = 8
)

// ErrCode is a used, expired or malformed code; a page says that much and no more.
var ErrCode = errors.New("users: that code was already used, is older than five minutes, or is not one")

// NewToken draws a session token.
func NewToken() ([TokenSize]byte, error) {
	var t [TokenSize]byte
	_, err := rand.Read(t[:])
	return t, err
}

// TokenText is the cookie's value for a token; ParseToken reads it back.
func TokenText(t [TokenSize]byte) string { return base64.RawURLEncoding.EncodeToString(t[:]) }

// ParseToken reads a cookie's value; false for anything but a token.
func ParseToken(s string) ([TokenSize]byte, bool) {
	var t [TokenSize]byte
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != TokenSize {
		return t, false
	}
	copy(t[:], b)
	return t, true
}

// codeCipher is AES-256-GCM under the code key derived from the cluster key.
func codeCipher(key []byte) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, key, nil, "tailraid web code v2", 32)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// MintCode seals token in a code good for CodeLife from now under key.
func MintCode(key []byte, token [TokenSize]byte, now time.Time) (string, error) {
	aead, err := codeCipher(key)
	if err != nil {
		return "", err
	}
	var nonce [codeNonce]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	var plain [8 + TokenSize]byte
	binary.LittleEndian.PutUint64(plain[:], uint64(now.Add(CodeLife).Unix())) //nolint:gosec // a unix time
	copy(plain[8:], token[:])
	b := make([]byte, 0, codeLen)
	b = append(b, nonce[:]...)
	b = aead.Seal(b, nonce[:], plain[:], nil)
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// OpenCode opens a code under key at now and returns its token and its nonce, which the
// CodeLog remembers. Every refusal is ErrCode.
func OpenCode(key []byte, code string, now time.Time) (token [TokenSize]byte, nonce [CodeNonceLen]byte, expires time.Time, err error) {
	b, derr := base64.RawURLEncoding.DecodeString(code)
	if derr != nil || len(b) != codeLen {
		return token, nonce, expires, ErrCode
	}
	aead, err := codeCipher(key)
	if err != nil {
		return token, nonce, expires, ErrCode
	}
	plain, err := aead.Open(nil, b[:codeNonce], b[codeNonce:], nil)
	if err != nil || len(plain) != 8+TokenSize {
		return token, nonce, expires, ErrCode
	}
	expires = time.Unix(int64(binary.LittleEndian.Uint64(plain[:8])), 0) //nolint:gosec // a unix time
	if !now.Before(expires) {
		return token, nonce, expires, ErrCode
	}
	copy(nonce[:], b[:CodeNonceLen])
	copy(token[:], plain[8:])
	return token, nonce, expires, nil
}

// CodeLog is the codes one node has opened, each kept until it expires (I-354).
type CodeLog struct {
	mu   sync.Mutex
	seq  uint64
	used map[[CodeNonceLen]byte]codeUse
}

type codeUse struct {
	expires time.Time
	seq     uint64 // the order of opening: the oldest goes first past CodeLogMax
}

// Use records nonce as opened; false when it was already.
func (l *CodeLog) Use(nonce [CodeNonceLen]byte, expires, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used == nil {
		l.used = map[[CodeNonceLen]byte]codeUse{}
	}
	for n, u := range l.used {
		if !now.Before(u.expires) {
			delete(l.used, n)
		}
	}
	if _, seen := l.used[nonce]; seen {
		return false
	}
	for len(l.used) >= CodeLogMax {
		var oldest [CodeNonceLen]byte
		first := true
		var oldestSeq uint64
		for n, u := range l.used {
			if first || u.seq < oldestSeq {
				oldest, oldestSeq, first = n, u.seq, false
			}
		}
		delete(l.used, oldest)
	}
	l.seq++
	l.used[nonce] = codeUse{expires: expires, seq: l.seq}
	return true
}
