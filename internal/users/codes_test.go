package users

import (
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
		var n [codeNonce]byte
		n[0], n[1] = byte(i), byte(i>>8)
		if !big.Use(n, exp, now) {
			t.Fatalf("code %d refused", i)
		}
	}
	if len(big.used) != CodeLogMax {
		t.Fatalf("log holds %d", len(big.used))
	}
	if !big.Use([codeNonce]byte{0, 0}, exp, now) { // the oldest was dropped: it opens again
		t.Fatal("the oldest code was kept")
	}
}
