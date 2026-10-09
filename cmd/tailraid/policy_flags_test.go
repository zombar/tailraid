package main

import "testing"

// --k and --m are checked before the narrowing to uint8: k is 1..255, m is 0..255 and
// k+m is at most 256. --k 257 --m 0 used to pin 1+0 after the cast.
func TestPolicyFlags_Bounds(t *testing.T) {
	for _, c := range []struct {
		k, m int
		ok   bool
	}{
		{0, 0, false},
		{1, 0, true},
		{1, 255, true},
		{2, 254, true},
		{2, 255, false},
		{255, 0, true},
		{255, 1, true},
		{256, 0, false},
		{257, 0, false},
		{1, 256, false},
		{1, -1, false},
		{-1, 1, false},
	} {
		kk, mm, err := policyFlags(c.k, c.m)
		if (err == nil) != c.ok {
			t.Errorf("k=%d m=%d: err=%v, want ok=%v", c.k, c.m, err, c.ok)
		}
		if err == nil && (int(kk) != c.k || int(mm) != c.m) {
			t.Errorf("k=%d m=%d narrowed to %d+%d", c.k, c.m, kk, mm)
		}
	}
}
