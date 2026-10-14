package config

import "testing"

// mount.owner is unset or uid:gid; anything else is refused at Validate.
// Invariant: I-324
func TestMountOwner_Table(t *testing.T) {
	cases := []struct {
		in       string
		uid, gid uint32
		ok       bool
	}{
		{"", 0, 0, true},
		{"501:20", 501, 20, true},
		{"0:0", 0, 0, true},
		{"501", 0, 0, false},
		{"a:b", 0, 0, false},
		{"1:2:3", 0, 0, false},
		{"-1:2", 0, 0, false},
	}
	for _, tc := range cases {
		c := Default()
		c.Mount.Owner = tc.in
		uid, gid, err := c.MountOwner()
		if (err == nil) != tc.ok || uid != tc.uid || gid != tc.gid {
			t.Errorf("%q: %d %d %v", tc.in, uid, gid, err)
		}
		if verr := c.Validate(); (verr == nil) != tc.ok {
			t.Errorf("%q: Validate %v", tc.in, verr)
		}
	}
}
