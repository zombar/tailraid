package mesh

import (
	"errors"
	"net/netip"
	"testing"
)

// Invariant: I-320
func TestSubnet_FromClusterID_Table(t *testing.T) {
	cases := []struct {
		id   [16]byte
		want string
	}{
		{[16]byte{0, 0}, "100.64.0.0/24"},
		{[16]byte{63, 255}, "100.127.255.0/24"},
		{[16]byte{64, 1}, "100.64.1.0/24"},
		{[16]byte{255, 7, 9, 9}, "100.127.7.0/24"},
		{[16]byte{0x3a, 0xb5, 0x12, 0x32}, "100.122.181.0/24"},
	}
	for _, tc := range cases {
		got := Subnet(tc.id)
		if got.String() != tc.want {
			t.Errorf("Subnet(%x) = %s, want %s", tc.id[:2], got, tc.want)
		}
		if !meshSpace.Contains(got.Addr()) {
			t.Errorf("Subnet(%x) = %s is outside %s", tc.id[:2], got, meshSpace)
		}
		// the same on every node: a pure function of the ID
		for range 3 {
			if Subnet(tc.id) != got {
				t.Fatal("Subnet is not deterministic")
			}
		}
	}
}

// Invariant: I-320
func TestSubnet_HostAddr_Bounds(t *testing.T) {
	sub := netip.MustParsePrefix("100.70.3.0/24")
	cases := []struct {
		n    uint8
		want string
		ok   bool
	}{
		{0, "", false},
		{1, "100.70.3.1", true},
		{2, "100.70.3.2", true},
		{253, "100.70.3.253", true},
		{254, "100.70.3.254", true},
		{255, "", false},
	}
	for _, tc := range cases {
		got, err := HostAddr(sub, tc.n)
		if tc.ok != (err == nil) {
			t.Errorf("HostAddr(%d): err=%v", tc.n, err)
			continue
		}
		if !tc.ok {
			if !errors.Is(err, ErrHost) {
				t.Errorf("HostAddr(%d) = %v, want ErrHost", tc.n, err)
			}
			continue
		}
		if got.String() != tc.want {
			t.Errorf("HostAddr(%d) = %s, want %s", tc.n, got, tc.want)
		}
	}
	// a non-masked prefix still gives the subnet's host
	if got, _ := HostAddr(netip.MustParsePrefix("100.70.3.77/24"), 9); got.String() != "100.70.3.9" {
		t.Errorf("unmasked: %s", got)
	}
}
