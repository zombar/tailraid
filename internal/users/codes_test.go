package users

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// A code opens once under its key within CodeLife and carries its token; a second
// opening, another key, a flipped byte, a short string or the time past its expiry is
// ErrCode; the log keeps at most CodeLogMax codes, dropping the oldest.
// Invariant: I-354
func TestCodes_OnceWithinLife(t *testing.T) {
	key := []byte("k")
	now := time.Unix(1_700_000_000, 0)
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	code, err := MintCode(key, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	got, nonce, exp, err := OpenCode(key, code, now.Add(CodeLife-time.Second))
	if err != nil || got != tok || !exp.Equal(now.Add(CodeLife)) {
		t.Fatalf("open: %v %v", err, exp)
	}
	var log CodeLog
	if !log.Use(nonce, exp, now) || log.Use(nonce, exp, now) {
		t.Fatal("the log took the code twice")
	}
	flipped := []byte(code)
	flipped[len(flipped)/2] ^= 1
	for name, c := range map[string]struct {
		key  []byte
		code string
		at   time.Time
	}{
		"expired":   {key, code, now.Add(CodeLife)},
		"other key": {[]byte("j"), code, now},
		"flipped":   {key, string(flipped), now},
		"short":     {key, code[:10], now},
		"empty":     {key, "", now},
	} {
		if _, _, _, err := OpenCode(c.key, c.code, c.at); !errors.Is(err, ErrCode) {
			t.Errorf("%s: %v, want ErrCode", name, err)
		}
	}
	if s := TokenText(tok); s == "" {
		t.Fatal("no text")
	} else if back, ok := ParseToken(s); !ok || back != tok {
		t.Fatal("token text does not round-trip")
	}
	if _, ok := ParseToken("not a token"); ok {
		t.Fatal("junk parsed as a token")
	}
	// the log is bounded: CodeLogMax+1 codes keep the newest CodeLogMax
	var big CodeLog
	exp = now.Add(time.Hour)
	for i := range CodeLogMax + 1 {
		var n [CodeNonceLen]byte
		n[0], n[1] = byte(i), byte(i>>8)
		if !big.Use(n, exp, now) {
			t.Fatalf("code %d refused", i)
		}
	}
	if len(big.used) != CodeLogMax {
		t.Fatalf("log holds %d", len(big.used))
	}
	if !big.Use([CodeNonceLen]byte{0, 0}, exp, now) { // the oldest was dropped: it opens again
		t.Fatal("the oldest code was kept")
	}
}

// A code carries the token sealed, not in the clear: the code's bytes hold neither the
// token nor its expiry, two codes for one token differ, and a code opens under the key
// it was minted under alone (docs/21 §6).
// Invariant: I-354
func TestCodes_TokenNotRecoverableFromCode(t *testing.T) {
	key := []byte("the cluster key")
	now := time.Unix(1_700_000_000, 0)
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	code, err := MintCode(key, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		t.Fatal(err)
	}
	for n := 8; n <= len(tok); n += 8 {
		if bytes.Contains(raw, tok[:n]) || bytes.Contains(raw, tok[len(tok)-n:]) {
			t.Fatalf("%d bytes of the token appear in the code", n)
		}
	}
	var exp [8]byte
	binary.LittleEndian.PutUint64(exp[:], uint64(now.Add(CodeLife).Unix()))
	if bytes.Contains(raw, exp[:]) {
		t.Fatal("the expiry appears in the code in the clear")
	}
	again, err := MintCode(key, tok, now)
	if err != nil || again == code {
		t.Fatalf("two codes for one token: %v %v", again == code, err)
	}
	if got, _, _, err := OpenCode(key, again, now); err != nil || got != tok {
		t.Fatalf("the second code: %v", err)
	}
}
