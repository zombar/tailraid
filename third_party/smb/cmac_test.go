package smb

import (
	"encoding/hex"
	"testing"
)

// The test vectors from RFC 4493 section 4, which is what makes this the same
// function as everyone else's -- a CMAC that is nearly right signs messages
// that no client will accept.
func TestCMACAgainstRFC4493(t *testing.T) {
	key, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	msg, _ := hex.DecodeString(
		"6bc1bee22e409f96e93d7e117393172a" +
			"ae2d8a571e03ac9c9eb76fac45af8e51" +
			"30c81c46a35ce411e5fbc1191a0a52ef" +
			"f69f2445df4f9b17ad2b417be66c3710")
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "bb1d6929e95937287fa37d129b756746"},
		{16, "070a16b46b4d4144f79bdd9dd04a287c"},
		{40, "dfa66747de9ae63030ca32611497c827"},
		{64, "51f0bebf7e3b9d92fc49741779363cfe"},
	} {
		if got := hex.EncodeToString(cmacSum(key, msg[:tc.n])); got != tc.want {
			t.Errorf("cmac over %d bytes = %s, want %s", tc.n, got, tc.want)
		}
	}
}

// A key of the wrong length is a programming error, and it says so rather than
// signing with something else.
func TestCMACRefusesAKeyThatIsNotOne(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a seven-byte key was accepted")
		}
	}()
	cmacSum(make([]byte, 7), nil)
}
