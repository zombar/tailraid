package config

import "testing"

// staging.size: derived (0) unless set, else at least 1 GiB, in the units ParseBytes reads
// (docs/04 §3, docs/12 §8).
//
// Invariant: I-396
func TestConfig_StagingSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want uint64
		bad  bool
	}{
		{"", 0, false},
		{"1G", 1 << 30, false},
		{"1073741823", 0, true},
		{"1073741824", 1 << 30, false},
		{"1073741825", 1<<30 + 1, false},
		{"200GiB", 200 << 30, false},
		{"0", 0, true},
		{"lots", 0, true},
	} {
		cfg := Default()
		cfg.Staging.Size = c.in
		got, err := cfg.StagingSize()
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("%q: %d %v, want %d (bad %v)", c.in, got, err, c.want, c.bad)
		}
		if verr := cfg.Validate(); (verr != nil) != c.bad {
			t.Errorf("%q: Validate %v, want refused %v", c.in, verr, c.bad)
		}
	}
}
