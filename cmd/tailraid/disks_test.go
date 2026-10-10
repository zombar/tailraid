package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// init registers the disks the config lists; `node add-disk` marks, saves, registers and
// opens one more on the running node (docs/14 §2).
// Invariant: I-86, I-90
func TestCLI_InitWithDisks_AndAddDisk(t *testing.T) {
	path, cfg := devHome(t)
	home := filepath.Dir(path)
	cfg.Node.Disks = []config.Disk{{Path: filepath.Join(home, "d1"), Capacity: "1G"}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "init", "--config", path, "--token", "testtoken")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	host, _ := os.Hostname()
	if !strings.Contains(out, "disk "+node.DiskName(host, 1)) {
		t.Fatalf("init output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "d1", node.MarkerName)); err != nil {
		t.Fatalf("init wrote no marker: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- node.Serve(ctx, cfg, slog.Default(), "test") }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Listen.RPC)
	probe := rpc.NewClient([16]byte{}, nil)
	testharness.Eventually(t, func() bool { h, err := probe.Health(ctx, addr); return err == nil && h.Ready }, "node never became ready")

	// a non-empty path is refused before anything is registered
	junk := filepath.Join(home, "junk")
	_ = os.MkdirAll(junk, 0o700)
	_ = os.WriteFile(filepath.Join(junk, "file"), []byte("x"), 0o600)
	if out, err := run(t, "node", "add-disk", junk, "--config", path); err == nil {
		t.Fatalf("add-disk on a non-empty path: %s", out)
	}
	out, err = run(t, "node", "add-disk", filepath.Join(home, "d2"), "--capacity", "1G", "--config", path)
	if err != nil {
		t.Fatalf("add-disk: %v\n%s", err, out)
	}
	if !strings.Contains(out, node.DiskName(host, 2)) || !strings.Contains(out, ": open") {
		t.Fatalf("add-disk output: %s", out)
	}
	saved, err := config.Load(path)
	if err != nil || len(saved.Node.Disks) != 2 {
		t.Fatalf("saved config disks %+v %v", saved.Node.Disks, err)
	}
	cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	c := rpc.NewClient(cid, nil)
	testharness.Eventually(t, func() bool {
		rep, err := admin.FetchStatus(ctx, c, addr)
		if err != nil {
			return false
		}
		live := 0
		for _, n := range rep.Nodes {
			if n.Kind == "disk" && n.Live && n.Self {
				live++
			}
		}
		return live == 2
	}, "both disks live in status")
}
