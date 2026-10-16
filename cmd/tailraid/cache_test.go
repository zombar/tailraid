package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/node"
)

// The cache commands reach this node's pins (docs/12 §10): an empty list reads as such
// with the cache's size, the root and a missing folder refuse to pin, and an unpin of
// nothing fails naming the path.
//
// Invariant: I-162, I-373
func TestCLI_Cache_PinUnpinLs(t *testing.T) {
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
	waitReady(context.Background(), t, addr, "the node never served") // a CI runner takes longer than Eventually allows (docs/09 §5)

	out, err := run(t, "cache", "ls", "--config", path)
	if err != nil || !strings.Contains(out, "no pinned folders") || !strings.Contains(out, "of 1.0 GiB") {
		t.Fatalf("cache ls: %q %v", out, err)
	}
	// the system pin comes first, with its budget (I-373)
	if !strings.Contains(out, "/.tailraid (system, up to 1.0 GiB)") || strings.Index(out, "/.tailraid") > strings.Index(out, "no pinned folders") {
		t.Fatalf("cache ls without the system pin first: %q", out)
	}
	if out, err := run(t, "cache", "ls", "--json", "--config", path); err != nil || !strings.Contains(out, `"pins": []`) || !strings.Contains(out, `"system": {`) {
		t.Fatalf("cache ls --json: %q %v", out, err)
	}
	if _, err := run(t, "cache", "pin", "/", "--config", path); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("pin of the root: %v", err)
	}
	if _, err := run(t, "cache", "pin", "/nope", "--config", path); err == nil || !strings.Contains(err.Error(), "no such") {
		t.Fatalf("pin of nothing: %v", err)
	}
	if _, err := run(t, "cache", "unpin", "/nope", "--config", path); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("unpin of nothing: %v", err)
	}
}
