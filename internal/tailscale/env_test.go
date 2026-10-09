package tailscale

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The macOS app's bundled binary acts as the CLI only when TERM is set; under launchd
// (no TERM) it tries to start the GUI and prints "The Tailscale GUI failed to start", so
// a service never got past "waiting for tailscale". The child always gets a TERM.
func TestCLI_ChildGetsTERM(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"PATH=/bin"}, "dumb"},
		{[]string{"PATH=/bin", "TERM="}, "dumb"},
		{[]string{"TERM=xterm-256color", "PATH=/bin"}, "xterm-256color"},
	}
	for _, c := range cases {
		got := ""
		for _, kv := range cliEnv(c.in) {
			if len(kv) > 5 && kv[:5] == "TERM=" {
				got = kv[5:]
			}
		}
		if got != c.want {
			t.Errorf("cliEnv(%v): TERM=%q, want %q", c.in, got, c.want)
		}
	}
	// end to end: a stub that refuses to answer without TERM, run with none set
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	p := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\n[ -n \"$TERM\" ] || { echo 'The Tailscale GUI failed to start'; exit 0; }\necho '{\"BackendState\":\"Running\"}'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "")
	if _, err := (&CLI{Path: p}).Status(ctx); err != nil {
		t.Fatalf("status without TERM in the parent: %v", err)
	}
}
