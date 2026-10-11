package config

import (
	"testing"
	"time"
)

// trash.retention: 30 days unless set, "0" for never, else at least an hour (docs/12 §10).
//
// Invariant: I-157
func TestConfig_TrashRetention(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{{"", 30 * 24 * time.Hour, false}, {"0", 0, false}, {"1h", time.Hour, false}, {"720h", 720 * time.Hour, false}, {"59m59s", 0, true}, {"-1h", 0, true}, {"soon", 0, true}} {
		cfg := Default()
		cfg.Trash.Retention = c.in
		got, err := cfg.TrashRetention()
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("%q: %v %v, want %v (bad %v)", c.in, got, err, c.want, c.bad)
		}
	}
}
