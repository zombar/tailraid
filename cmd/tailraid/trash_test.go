package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// The trash commands reach the leader's admin ops (docs/12 §10): an empty trash lists
// as such, a purge of everything reports zero, a restore of nothing fails naming the
// entry, and the purge refuses to run without a selection.
//
// Invariant: I-157
func TestCLI_Trash_LsRestorePurge(t *testing.T) {
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
	probe := rpc.NewClient([16]byte{}, nil)
	testharness.Eventually(t, func() bool { h, err := probe.Health(context.Background(), addr); return err == nil && h.Ready }, "the node never served")

	if out, err := run(t, "trash", "ls", "--config", path); err != nil || !strings.Contains(out, "the trash is empty") {
		t.Fatalf("trash ls: %q %v", out, err)
	}
	if out, err := run(t, "trash", "purge", "--all", "--config", path); err != nil || !strings.Contains(out, `"purged": 0`) {
		t.Fatalf("trash purge --all: %q %v", out, err)
	}
	if _, err := run(t, "trash", "purge", "--config", path); err == nil || !strings.Contains(err.Error(), "--all") {
		t.Fatalf("trash purge without a selection: %v", err)
	}
	if _, err := run(t, "trash", "restore", "999", "--config", path); err == nil || !strings.Contains(err.Error(), "no such entry") {
		t.Fatalf("trash restore of nothing: %v", err)
	}
	if _, err := run(t, "trash", "restore", "x", "--config", path); err == nil || !strings.Contains(err.Error(), "inode") {
		t.Fatalf("trash restore x: %v", err)
	}
}
