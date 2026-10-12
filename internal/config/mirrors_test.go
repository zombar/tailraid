package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Invariant: I-182, I-188
func TestConfig_Mirrors(t *testing.T) {
	def, err := Default().Mirroring()
	if err != nil {
		t.Fatal(err)
	}
	if want := (MirrorPolicy{Every: time.Hour, Hourly: 24, Daily: 30, Weekly: 8, Monthly: 12, Rate: 50 << 20, HoldMax: 24 * time.Hour, StaleAfter: 168 * time.Hour}); def != want {
		t.Fatalf("defaults %+v, want %+v", def, want)
	}
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name string
		set  func(*Config)
		bad  bool
		want func(MirrorPolicy) bool
	}{
		{"every 0 takes none", func(c *Config) { c.Mirrors.Every = "0" }, false, func(p MirrorPolicy) bool { return p.Every == 0 }},
		{"every 59s", func(c *Config) { c.Mirrors.Every = "59s" }, true, nil},
		{"every 1m", func(c *Config) { c.Mirrors.Every = "1m" }, false, func(p MirrorPolicy) bool { return p.Every == time.Minute }},
		{"keep 0", func(c *Config) { c.Mirrors.Keep.Hourly = n(0) }, false, func(p MirrorPolicy) bool { return p.Hourly == 0 && p.Daily == 30 }},
		{"keep max", func(c *Config) { c.Mirrors.Keep.Daily = n(MaxKeep) }, false, func(p MirrorPolicy) bool { return p.Daily == MaxKeep }},
		{"keep max+1", func(c *Config) { c.Mirrors.Keep.Weekly = n(MaxKeep + 1) }, true, nil},
		{"keep -1", func(c *Config) { c.Mirrors.Keep.Monthly = n(-1) }, true, nil},
		{"hold_max 59m", func(c *Config) { c.Mirrors.HoldMax = "59m" }, true, nil},
		{"hold_max 0", func(c *Config) { c.Mirrors.HoldMax = "0" }, true, nil},
		{"rate 0", func(c *Config) { c.Mirrors.Rate = "0" }, false, func(p MirrorPolicy) bool { return p.Rate == 0 }},
		{"rate bad", func(c *Config) { c.Mirrors.Rate = "fast" }, true, nil},
	} {
		cfg := Default()
		c.set(&cfg)
		got, err := cfg.Mirroring()
		if (err != nil) != c.bad || (err == nil && !c.want(got)) {
			t.Errorf("%s: %+v %v", c.name, got, err)
		}
	}

	// paths: absolute, not twice, not a disk, not the data directory
	root := t.TempDir() // absolute on every OS (a Windows path needs its volume)
	data, disk, usb := filepath.Join(root, "data"), filepath.Join(root, "a"), filepath.Join(root, "usb")
	base := Default()
	base.Node.DataDir = data
	base.Node.Disks = []Disk{{Path: disk}}
	for _, c := range []struct {
		path string
		bad  bool
	}{{usb, false}, {"relative", true}, {disk, true}, {data, true}} {
		cfg := base
		cfg.Node.Mirrors = []Mirror{{Path: c.path}}
		if err := cfg.Validate(); (err != nil) != c.bad {
			t.Errorf("mirror at %q: %v", c.path, err)
		}
	}

	// the keys are written only when set, so a release from before them reads the file
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := Save(p, base); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "mirrors") {
		t.Fatalf("an unset mirrors section was written:\n%s", b)
	}
	base.Node.Mirrors = []Mirror{{Path: usb}}
	base.Mirrors.Every = "2h"
	if err := Save(p, base); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Node.Mirrors) != 1 || got.Mirrors.Every != "2h" {
		t.Fatalf("round trip %+v %+v", got.Node.Mirrors, got.Mirrors)
	}
}
