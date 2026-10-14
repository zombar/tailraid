package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Validate refuses a port out of range or shared between listeners, an unknown log
// level or format and a relative mount path; the defaults and the documented values
// pass.
//
// Invariant: I-126
func TestConfig_Validate_Table(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*Config)
		want string // "" = valid, else a substring of the error
	}{
		{"defaults", nil, ""},
		{"raft port 0", func(c *Config) { c.Listen.Raft = 0 }, "listen.raft 0"},
		{"rpc port 65536", func(c *Config) { c.Listen.RPC = 65536 }, "listen.rpc 65536"},
		{"rpc port 65535", func(c *Config) { c.Listen.RPC = 65535 }, ""},
		{"raft equals rpc", func(c *Config) { c.Listen.RPC = c.Listen.Raft }, "both use port"},
		{"nfs equals raft", func(c *Config) { c.Listen.NFS = "127.0.0.1:20480" }, "both use port"},
		{"nfs without a port", func(c *Config) { c.Listen.NFS = "127.0.0.1" }, "listen.nfs"},
		{"nfs port 0", func(c *Config) { c.Listen.NFS = "127.0.0.1:0" }, "listen.nfs"},
		{"web equals nfs", func(c *Config) { c.Listen.NFS, c.Listen.Web = "127.0.0.1:20490", 20490 }, "both use port"}, // the NFS default differs per OS
		{"web off", func(c *Config) { c.Listen.Web = 0 }, ""},
		{"level warning", func(c *Config) { c.Log.Level = "warning" }, ""},
		{"level DEBUG", func(c *Config) { c.Log.Level = "DEBUG" }, ""},
		{"level loud", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		{"format JSON", func(c *Config) { c.Log.Format = "JSON" }, ""},
		{"format yaml", func(c *Config) { c.Log.Format = "yaml" }, "log.format"},
		{"mount relative", func(c *Config) { c.Mount.Path = "TailRAID" }, "mount.path"},
		{"mount unexpanded tilde", func(c *Config) { c.Mount.Path = "~/TailRAID" }, "mount.path"},
		{"mount empty", func(c *Config) { c.Mount.Path = "" }, ""},
		{"trash retention default", nil, ""},
		{"trash retention never", func(c *Config) { c.Trash.Retention = "0" }, ""},
		{"trash retention 1h", func(c *Config) { c.Trash.Retention = "1h" }, ""},
		{"trash retention 59m", func(c *Config) { c.Trash.Retention = "59m" }, "trash.retention"},
		{"trash retention words", func(c *Config) { c.Trash.Retention = "a month" }, "trash.retention"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			cfg.Node.DataDir = filepath.Join(t.TempDir(), "data")
			if c.mut != nil {
				c.mut(&cfg)
			}
			err := cfg.Validate()
			if runtime.GOOS == "windows" && strings.HasPrefix(c.name, "mount") && c.want != "" {
				c.want = "" // drive letters are not absolute paths; the mount rule is Unix-only
			}
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("valid config refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
}

// A leading ~ in every path the config names is the home directory after Load.
//
// Invariant: I-126
func TestConfig_ExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("node:\n  data_dir: ~/data\n  disks:\n    - path: ~/disk\nmount:\n  path: ~/TailRAID\nlog:\n  file: ~/logs/serve.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{c.Node.DataDir, c.Node.Disks[0].Path, c.Mount.Path, c.Log.File} {
		if !strings.HasPrefix(got, home+string(filepath.Separator)) {
			t.Fatalf("%q not expanded under %q", got, home)
		}
	}
	if got := ExpandHome("~"); got != home {
		t.Fatalf("bare ~: %q", got)
	}
	if got := ExpandHome("/abs/~/x"); got != "/abs/~/x" {
		t.Fatalf("an inner ~ is not expanded: %q", got)
	}
	if got := ExpandHome("~user/x"); got != "~user/x" {
		t.Fatalf("~user is not ours to expand: %q", got)
	}
}

// serve warns when the config file or the data directory is readable by other users.
//
// Invariant: I-126
func TestConfig_Warnings_Permissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	c := Default()
	c.Node.DataDir = filepath.Join(dir, "data")
	if err := os.WriteFile(p, []byte("node: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.Node.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w := c.Warnings(p)
	if len(w) != 2 || !strings.Contains(w[0], "chmod 600") || !strings.Contains(w[1], "chmod 700") {
		t.Fatalf("warnings %v", w)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.Node.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(p); len(w) != 0 {
		t.Fatalf("warnings after chmod: %v", w)
	}
	c.Node.DataDir = filepath.Join(dir, "missing")
	if w := c.Warnings(p); len(w) != 0 {
		t.Fatalf("a missing data directory is init's problem, not a warning: %v", w)
	}
}

// Every disk and mirror path gets the data directory's rule: a mode that lets other
// users in is one line naming chmod 700; 0700 is silent; a missing path says nothing.
// Invariant: I-341
func TestConfig_Warnings_PathsTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("node: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"disk", "mirror"} {
		for _, mode := range []os.FileMode{0o700, 0o750, 0o755, 0o770, 0o777} {
			c := Default()
			c.Node.DataDir = filepath.Join(dir, "data")
			if err := os.MkdirAll(c.Node.DataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, fmt.Sprintf("%s-%o", kind, mode))
			if err := os.Mkdir(target, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, mode); err != nil { // Mkdir applies the umask
				t.Fatal(err)
			}
			switch kind {
			case "disk":
				c.Node.Disks = []Disk{{Path: target}}
			case "mirror":
				c.Node.Mirrors = []Mirror{{Path: target}}
			}
			w := c.Warnings(p)
			wide := mode&0o077 != 0
			if wide && (len(w) != 1 || !strings.Contains(w[0], kind+" "+target) || !strings.Contains(w[0], "chmod 700")) {
				t.Fatalf("%s %o: warnings %v", kind, mode, w)
			}
			if !wide && len(w) != 0 {
				t.Fatalf("%s %o: unexpected warnings %v", kind, mode, w)
			}
		}
		c := Default()
		c.Node.DataDir = filepath.Join(dir, "data")
		c.Node.Disks = []Disk{{Path: filepath.Join(dir, "missing-"+kind)}}
		if w := c.Warnings(p); len(w) != 0 {
			t.Fatalf("a missing %s is init's problem, not a warning: %v", kind, w)
		}
	}
}
