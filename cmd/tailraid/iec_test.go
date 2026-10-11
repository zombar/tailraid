package main

import "testing"

// iec picks the unit the value fits: a byte count under 1024 stays in bytes, each
// power of 1024 steps one unit, and the top unit holds the rest.
func TestIEC_Units(t *testing.T) {
	for _, c := range []struct {
		n    uint64
		want string
	}{{0, "0 B"}, {1023, "1023 B"}, {1024, "1.0 KiB"}, {1536, "1.5 KiB"}, {1 << 20, "1.0 MiB"}, {1 << 30, "1.0 GiB"}, {1 << 40, "1.0 TiB"}, {1 << 60, "1.0 EiB"}, {^uint64(0), "16.0 EiB"}} {
		if got := iec(c.n); got != c.want {
			t.Errorf("iec(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
