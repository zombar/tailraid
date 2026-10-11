package config

import "testing"

// cache.size: 1 GiB unless set, else at least 64 MiB, in the units ParseBytes reads
// (docs/12 §10).
//
// Invariant: I-162
func TestConfig_CacheSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{"", 1 << 30, false},
		{"64M", 64 << 20, false},
		{"64MiB", 64 << 20, false},
		{"67108864", 64 << 20, false},
		{"67108863", 0, true},
		{"67108865", 64<<20 + 1, false},
		{"2G", 2 << 30, false},
		{"0", 0, true},
		{"-1G", 0, true},
		{"lots", 0, true},
	} {
		cfg := Default()
		cfg.Cache.Size = c.in
		got, err := cfg.CacheSize()
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("%q: %d %v, want %d (bad %v)", c.in, got, err, c.want, c.bad)
		}
		if verr := cfg.Validate(); (verr != nil) != c.bad {
			t.Errorf("%q: Validate %v, want refused %v", c.in, verr, c.bad)
		}
	}
}
