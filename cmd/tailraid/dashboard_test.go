package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/node"
)

// Each OS opens the URL with its own opener and the URL as one argument, never through a
// shell, so nothing in it is interpreted; an unknown OS is an error, not a guess.
// Invariant: I-143
func TestBrowserCommand_PerOS(t *testing.T) {
	const u = "http://100.64.0.3:20482/"
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"darwin", []string{"open", u}},
		{"linux", []string{"xdg-open", u}},
		{"windows", []string{"rundll32", "url.dll,FileProtocolHandler", u}},
	} {
		got, err := browserCommand(tc.goos, u)
		if err != nil || strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: %q %v, want %q", tc.goos, got, err, tc.want)
		}
	}
	if _, err := browserCommand("plan9", u); err == nil {
		t.Error("an OS without a known opener got a command")
	}
}

// dashboard --print prints the URL the serving node reports and opens nothing; a status
// with no web_url (listen.web: 0) fails naming listen.web.
// Invariant: I-143
func TestCLI_Dashboard(t *testing.T) {
	path, cfg := devHome(t)
	if out, err := run(t, "init", "--config", path, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- node.Serve(sctx, cfg, slog.Default(), "test") }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Listen.RPC)
	waitReady(context.Background(), t, addr, "the node never served")

	out, err := run(t, "dashboard", "--config", path, "--print")
	if want := fmt.Sprintf("http://127.0.0.1:%d/\n", cfg.Listen.Web); err != nil || out != want {
		t.Fatalf("dashboard --print: %q %v, want %q", out, err, want)
	}
	if _, err := dashboardURL(admin.Report{}); err == nil || !strings.Contains(err.Error(), "listen.web") {
		t.Errorf("no web_url: %v", err)
	}
}

// stats seal asks the serving node to write its partial hour now and prints how many
// rows it sealed; --json prints the reply.
// Invariant: I-192
func TestCLI_StatsSeal(t *testing.T) {
	path, cfg := devHome(t)
	if out, err := run(t, "init", "--config", path, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- node.Serve(sctx, cfg, slog.Default(), "test") }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Listen.RPC)
	waitReady(context.Background(), t, addr, "the node never served")

	out, err := run(t, "stats", "seal", "--config", path)
	if err != nil || !strings.HasPrefix(out, "sealed ") || !strings.HasSuffix(out, " rows\n") {
		t.Fatalf("stats seal: %q %v", out, err)
	}
	if out, err := run(t, "stats", "seal", "--config", path, "--json"); err != nil || !strings.Contains(out, `"sealed_rows"`) {
		t.Fatalf("stats seal --json: %q %v", out, err)
	}
}
