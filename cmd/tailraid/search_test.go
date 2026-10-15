package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/node"
)

// search asks the local node's index and prints its hits or "no matches"; --json prints
// the reply; a limit outside 1..200 is refused before the call; search index runs the
// leader's pass and prints what it did.
// Invariant: I-208
func TestCLI_Search(t *testing.T) {
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

	if out, err := run(t, "search", "index", "--config", path); err != nil || !strings.Contains(out, `"indexed"`) {
		t.Fatalf("search index: %q %v", out, err)
	}
	if out, err := run(t, "search", "nothing", "here", "--config", path); err != nil || out != "no matches for \"nothing here\"\n" {
		t.Fatalf("search: %q %v", out, err)
	}
	if out, err := run(t, "search", "nothing", "--config", path, "--json"); err != nil || !strings.Contains(out, `"hits"`) {
		t.Fatalf("search --json: %q %v", out, err)
	}
	for _, limit := range []string{"0", "201"} {
		if _, err := run(t, "search", "x", "--config", path, "--limit", limit); err == nil || !strings.Contains(err.Error(), "--limit") {
			t.Fatalf("--limit %s: %v", limit, err)
		}
	}
}
