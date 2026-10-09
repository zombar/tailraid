package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	cases := map[string]uint64{"4096": 4096, "100M": 100 << 20, "1.5G": 3 << 29, "2T": 2 << 40, "8gib": 8 << 30, "7 KB": 7 << 10}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "1Q", "-1G"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) accepted", bad)
		}
	}
}

func TestLoadSave_RoundTripAndStrict(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	c := Default()
	c.Node.Name = "box"
	c.Node.DataDir = filepath.Join(dir, "data")
	c.Node.Voter = true
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Node.Name != "box" || !got.Node.Voter || got.Listen.Raft != 20480 || got.Node.DataDir != c.Node.DataDir {
		t.Fatalf("round trip %+v", got)
	}
	// unknown keys are rejected
	if err := os.WriteFile(p, []byte("node:\n  name: x\n  data_dir: /tmp/x\n  colour: blue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("unknown key accepted: %v", err)
	}
	// partial files keep defaults
	if err := os.WriteFile(p, []byte("node:\n  data_dir: /tmp/y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = Load(p)
	if err != nil || (got.Listen.NFS == "" && got.Listen.SMB == "") || got.Node.DataDir != "/tmp/y" {
		t.Fatalf("partial: %+v %v", got, err)
	}
}

// A node serves at least one gateway; the SMB gateway needs the share password, which
// init and join generate once and never regenerate (docs/15 §6).
// Invariant: I-110
func TestConfig_Gateways(t *testing.T) {
	c := Default()
	c.Node.DataDir = "/tmp/d"
	c.Listen.NFS, c.Listen.SMB, c.SMB.Password = "", "", ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "listen.nfs or listen.smb") {
		t.Fatalf("no gateway: %v", err)
	}
	c.Listen.SMB = DefaultSMBAddr
	if err := c.Validate(); err != nil { // loadable without the password: init and join add it
		t.Fatalf("smb without a password: %v", err)
	}
	if !c.EnsureSMBPassword() || len(c.SMB.Password) != 24 || strings.Trim(c.SMB.Password, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
		t.Fatalf("generated password %q", c.SMB.Password)
	}
	first := c.SMB.Password
	if c.EnsureSMBPassword() || c.SMB.Password != first {
		t.Fatal("a password present was regenerated")
	}
	if NewSMBPassword() == first {
		t.Fatal("two passwords alike")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("smb only: %v", err)
	}
	c.Listen.NFS = "127.0.0.1:20490"
	if err := c.Validate(); err != nil {
		t.Fatalf("both gateways: %v", err)
	}
	c.Listen.SMB, c.SMB.Password = "", ""
	if c.EnsureSMBPassword() || c.SMB.Password != "" {
		t.Fatal("a password generated without listen.smb")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("nfs only: %v", err)
	}
}

func TestHome_RespectsEnv(t *testing.T) {
	h := filepath.Join(t.TempDir(), "x", "y")
	t.Setenv("TAILRAID_HOME", h)
	if Home() != h || DefaultPath() != filepath.Join(h, "config.yaml") {
		t.Fatal(Home(), DefaultPath())
	}
}

// Invariant: I-60
func TestConfig_LogFile(t *testing.T) {
	t.Setenv("TAILRAID_LOG_FORMAT", "")
	t.Setenv("TAILRAID_LOG_LEVEL", "")
	t.Setenv("TAILRAID_LOG_FILE", "")
	c := Default()
	c.Log.Format = "json"
	c.Log.File = filepath.Join(t.TempDir(), "logs", "serve.log")
	l, lf := c.OpenLogger()
	l.Info("hello", "k", 1)
	if err := lf.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(c.Log.File)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"msg":"hello","k":1`) {
		t.Fatalf("log file: %q", b)
	}
	l, lf = c.OpenLogger()
	l.Info("again")
	_ = lf.Close()
	if b, _ = os.ReadFile(c.Log.File); strings.Count(string(b), "\n") != 2 {
		t.Fatalf("not appended: %q", b)
	}
	t.Setenv("TAILRAID_LOG_FILE", filepath.Join(t.TempDir(), "env.log"))
	if c.LogFile() != os.Getenv("TAILRAID_LOG_FILE") {
		t.Fatal("env override ignored")
	}
	// a CLI command's logger never touches the file, neither the config's nor the env's
	c.Log.File = filepath.Join(t.TempDir(), "cli", "serve.log")
	c.Logger().Info("from a command")
	if _, err := os.Stat(c.Log.File); err == nil {
		t.Fatal("Logger() created log.file")
	}
	if _, err := os.Stat(os.Getenv("TAILRAID_LOG_FILE")); err == nil {
		t.Fatal("Logger() created the env log file")
	}
	if os.Getenv("TAILRAID_LOG_FILE") == "" {
		t.Fatal("Logger() lost the env override")
	}
}
