package config

import (
	"strings"
	"testing"
	"time"
)

// The search keys: defaults when unset, "0" turns the pass off, each bound at its edges.
// Invariant: I-207
func TestConfig_SearchKeys_Table(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string // "" = valid
	}{
		{"defaults", nil, ""},
		{"interval off", func(c *Config) { c.Search.Interval = "0" }, ""},
		{"interval 1m", func(c *Config) { c.Search.Interval = "1m" }, ""},
		{"interval 59s", func(c *Config) { c.Search.Interval = "59s" }, "search.interval"},
		{"interval junk", func(c *Config) { c.Search.Interval = "often" }, "search.interval"},
		{"max_file 64KiB", func(c *Config) { c.Search.MaxFile = "64KiB" }, ""},
		{"max_file 64KiB-1", func(c *Config) { c.Search.MaxFile = "65535" }, "search.max_file"},
		{"max_file 64MiB", func(c *Config) { c.Search.MaxFile = "64MiB" }, ""},
		{"max_file 64MiB+1", func(c *Config) { c.Search.MaxFile = "67108865" }, "search.max_file"},
		{"workers 1", func(c *Config) { c.Search.Workers = 1 }, ""},
		{"workers 16", func(c *Config) { c.Search.Workers = 16 }, ""},
		{"workers 17", func(c *Config) { c.Search.Workers = 17 }, "search.workers"},
		{"workers -1", func(c *Config) { c.Search.Workers = -1 }, "search.workers"},
		{"batch 1", func(c *Config) { c.Search.Batch = 1 }, ""},
		{"batch 100000", func(c *Config) { c.Search.Batch = 100000 }, ""},
		{"batch 100001", func(c *Config) { c.Search.Batch = 100001 }, "search.batch"},
		{"batch_bytes 1MiB", func(c *Config) { c.Search.BatchBytes = "1MiB" }, ""},
		{"batch_bytes 1MiB-1", func(c *Config) { c.Search.BatchBytes = "1048575" }, "search.batch_bytes"},
		{"max_bytes 16MiB", func(c *Config) { c.Search.MaxBytes = "16MiB" }, ""},
		{"max_bytes 16MiB-1", func(c *Config) { c.Search.MaxBytes = "16777215" }, "search.max_bytes"},
		{"max_bytes junk", func(c *Config) { c.Search.MaxBytes = "lots" }, "search.max_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			if tc.mut != nil {
				tc.mut(&c)
			}
			_, err := c.SearchSettings()
			if tc.want == "" && err != nil {
				t.Fatalf("valid config refused: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want an error naming %s", err, tc.want)
			}
		})
	}
	s, _ := Default().SearchSettings()
	if s != (SearchSettings{Interval: 10 * time.Minute, MaxFile: 1 << 20, Workers: 2, Batch: 2000, BatchBytes: 64 << 20, MaxBytes: 256 << 20}) {
		t.Fatalf("defaults %+v", s)
	}
	c := Default()
	c.Search.Interval = "0"
	if s, _ := c.SearchSettings(); s.Interval != 0 {
		t.Fatalf("off: %v", s.Interval)
	}
}
