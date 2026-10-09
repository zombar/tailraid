package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// serveInBackground runs node.Serve on cfg until the test ends and waits for its RPC port.
func serveInBackground(t *testing.T, cfg config.Config) (context.Context, string) {
	t.Helper()
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
	testharness.Eventually(t, func() bool { _, err := probe.Health(ctx, addr); return err == nil }, "node never served")
	return ctx, addr
}

// A state from before devices stopped being targets (a budget on the device row) makes
// serve return ErrLegacyState with the way out, and no listener ever opens (docs/14 §9).
// Invariant: I-119
func TestCLI_Serve_RefusesLegacyState(t *testing.T) {
	path, cfg := devHome(t)
	if out, err := run(t, "init", "--config", path, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	n := openFSM(t, cfg)
	self, _ := n.SelfIdx()
	row, _ := n.FSM.Node(self)
	row.Capacity = 1 << 30
	if _, _, err := n.Propose(context.Background(), meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: row}}); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := node.Serve(ctx, cfg, slog.Default(), "test")
	if !errors.Is(err, node.ErrLegacyState) || !strings.Contains(err.Error(), "tailraid reset --yes") {
		t.Fatalf("serve on a legacy state: %v", err)
	}
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", cfg.Listen.RPC), fmt.Sprintf("127.0.0.1:%d", cfg.Listen.Web), cfg.Listen.NFS} {
		if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			t.Fatalf("%s is listening after serve refused", addr)
		}
	}
}

// join marks and registers the disks its config lists, named <host>/<n> from 1, with a
// budget on each disk and none on the device row (docs/14 §9).
// Invariant: I-121
func TestCLI_Join_RegistersConfiguredDisks(t *testing.T) {
	pathA, cfgA := devHome(t)
	if out, err := run(t, "init", "--config", pathA, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	ctx, addrA := serveInBackground(t, cfgA)

	pathB, cfgB := devHome(t)
	home := filepath.Dir(pathB)
	cfgB.Node.Disks = []config.Disk{{Path: filepath.Join(home, "d1")}, {Path: filepath.Join(home, "d2"), Capacity: "1G"}}
	if err := config.Save(pathB, cfgB); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "join", "--config", pathB, addrA, "testtoken")
	if err != nil {
		t.Fatalf("join: %v\n%s", err, out)
	}
	host, _ := os.Hostname()
	for i := 1; i <= 2; i++ {
		if !strings.Contains(out, "disk "+node.DiskName(host, i)) {
			t.Fatalf("join output lacks disk %d:\n%s", i, out)
		}
		if _, err := os.Stat(filepath.Join(home, fmt.Sprintf("d%d", i), node.MarkerName)); err != nil {
			t.Fatalf("join wrote no marker on d%d: %v", i, err)
		}
	}
	if strings.Contains(out, node.NoDisksHint) {
		t.Fatalf("join with disks printed the no-disks hint:\n%s", out)
	}
	cid, err := cluster.LoadClusterID(cfgA.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := admin.FetchStatus(ctx, rpc.NewClient(cid, nil), addrA)
	if err != nil {
		t.Fatal(err)
	}
	var joined admin.NodeReport
	disks := map[string]admin.NodeReport{}
	for _, nd := range rep.Nodes {
		switch {
		case nd.Kind == "disk":
			disks[nd.Name] = nd
		case nd.Idx != rep.Nodes[0].Idx:
			joined = nd
		}
	}
	if joined.Diskless || joined.Capacity != 0 {
		t.Fatalf("joined device %+v: no budget, not diskless", joined)
	}
	if len(disks) != 2 {
		t.Fatalf("disks registered by join: %v", disks)
	}
	d1, d2 := disks[node.DiskName(host, 1)], disks[node.DiskName(host, 2)]
	if d1.Host != joined.Idx || d2.Host != joined.Idx || d1.Capacity == 0 || d2.Capacity != 1<<30 {
		t.Fatalf("disk rows %+v %+v: hosted by the joiner, one derived and one pinned to 1G", d1, d2)
	}
}

// init and join without disks print that the device stores nothing and how to add a
// disk; with a disk listed, neither does (docs/14 §9).
// Invariant: I-121
func TestCLI_Init_NoDisks_PrintsHint(t *testing.T) {
	pathA, cfgA := devHome(t)
	out, err := run(t, "init", "--config", pathA, "--token", "testtoken")
	if err != nil || !strings.Contains(out, node.NoDisksHint) {
		t.Fatalf("init without disks: %v\n%s", err, out)
	}
	_, addrA := serveInBackground(t, cfgA)
	pathB, _ := devHome(t)
	out, err = run(t, "join", "--config", pathB, addrA, "testtoken")
	if err != nil || !strings.Contains(out, node.NoDisksHint) {
		t.Fatalf("join without disks: %v\n%s", err, out)
	}
	pathC, cfgC := devHome(t)
	cfgC.Node.Disks = []config.Disk{{Path: filepath.Join(filepath.Dir(pathC), "d1")}}
	if err := config.Save(pathC, cfgC); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "init", "--config", pathC, "--token", "testtoken")
	if err != nil || strings.Contains(out, node.NoDisksHint) {
		t.Fatalf("init with a disk: %v\n%s", err, out)
	}
}
