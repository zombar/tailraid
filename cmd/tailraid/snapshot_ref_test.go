package main

import "testing"

// <mirror>/<name>: a mirror's name holds a slash, so the snapshot is what follows the
// last one (docs/12 §11).
//
// Invariant: I-188
func TestCLI_SplitSnapshotRef(t *testing.T) {
	for _, c := range []struct {
		ref, mirror, name string
		ok                bool
	}{
		{"pike/m1/2026-10-12T1400", "pike/m1", "2026-10-12T1400", true},
		{"pike-m1/s1", "pike-m1", "s1", true},
		{"s1", "", "", false},
		{"/s1", "", "", false},
		{"pike/m1/", "", "", false},
		{"", "", "", false},
	} {
		m, n, err := splitSnapshotRef(c.ref)
		if (err == nil) != c.ok || m != c.mirror || n != c.name {
			t.Errorf("%q → %q %q %v", c.ref, m, n, err)
		}
	}
}
