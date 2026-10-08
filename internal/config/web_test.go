package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Invariant: I-73
func TestConfig_ListenWeb(t *testing.T) {
	if Default().Listen.Web != 20482 {
		t.Fatalf("default listen.web = %d", Default().Listen.Web)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("node:\n  data_dir: /tmp/w\nlisten:\n  web: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || c.Listen.Web != 0 {
		t.Fatalf("web: 0 should load and disable the UI: %+v %v", c.Listen, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("web: 0 is valid: %v", err)
	}
	cases := []struct {
		name string
		web  int
		bad  string // substring of the error, "" = valid
	}{
		{"default", 20482, ""},
		{"off", 0, ""},
		{"max port", 65535, ""},
		{"too large", 65536, "listen.web"},
		{"negative", -1, "listen.web"},
		{"same as rpc", 20481, "listen.web"},
		{"same as raft", 20480, "listen.web"},
	}
	for _, tc := range cases {
		c := Default()
		c.Node.DataDir = "/tmp/w"
		c.Listen.Web = tc.web
		err := c.Validate()
		if tc.bad == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.bad != "" && (err == nil || !strings.Contains(err.Error(), tc.bad)) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.bad)
		}
	}
}
