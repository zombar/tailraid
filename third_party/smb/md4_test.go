package smb

import (
	"encoding/hex"
	"testing"
)

// The test vectors from RFC 1320 appendix A.5, which is what makes this
// implementation the same function as everyone else's -- an MD4 that is
// nearly right produces a password hash that is entirely wrong.
func TestMD4AgainstRFC1320(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "31d6cfe0d16ae931b73c59d7e0c089c0"},
		{"a", "bde52cb31de33e46245e05fbdbd6fb24"},
		{"abc", "a448017aaf21d8525fc10ae87aa6729d"},
		{"message digest", "d9130a8164549fe818874806e1c7014b"},
		{"abcdefghijklmnopqrstuvwxyz", "d79e1c308aa5bbcdeea8ed63df412da9"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", "043f8582f241db351ce627e153e7f0e4"},
		{"12345678901234567890123456789012345678901234567890123456789012345678901234567890", "e33b4ddc9c38f2199c3e7b164fcc0536"},
	} {
		if got := hex.EncodeToString(md4sum([]byte(tc.in))); got != tc.want {
			t.Errorf("md4(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A block boundary is where a padding mistake shows up, so the lengths either
// side of one are checked against a length-independent property: the same
// bytes always hash the same, and one different byte changes the answer.
func TestMD4AroundABlockBoundary(t *testing.T) {
	for n := 54; n <= 66; n++ {
		a := make([]byte, n)
		for i := range a {
			a[i] = byte(i)
		}
		b := append([]byte(nil), a...)
		if n > 0 {
			b[n-1] ^= 1
		}
		ha, hb := hex.EncodeToString(md4sum(a)), hex.EncodeToString(md4sum(b))
		if ha == hb {
			t.Errorf("length %d: one changed byte did not change the hash", n)
		}
		if again := hex.EncodeToString(md4sum(a)); again != ha {
			t.Errorf("length %d: the same input hashed differently twice", n)
		}
	}
}
