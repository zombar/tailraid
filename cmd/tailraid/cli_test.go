package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// The fake tailnet every dev home shares: loopback is this device, logged in as one user.
const fakeTailnet = `{"status":{"BackendState":"Running","TailscaleIPs":["127.0.0.1"],
 "Self":{"ID":"ts-dev","HostName":"dev","TailscaleIPs":["127.0.0.1"],"Online":true},"Peer":{}},
 "whois":{"127.0.0.1":{"Node":{"StableID":"ts-dev","Name":"dev.","Addresses":["127.0.0.1/32"]},"UserProfile":{"LoginName":"jim@example.com"}}}}`

func freePorts(t *testing.T, n int) []int {
	t.Helper()
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l.Addr().(*net.TCPAddr).Port)
		l.Close()
	}
	return out
}

// devHome writes a config with the fake tailnet and free ports; capacity stays derived.
func devHome(t *testing.T) (string, config.Config) {
	t.Helper()
	home := t.TempDir()
	fake := filepath.Join(home, "ts.json")
	if err := os.WriteFile(fake, []byte(fakeTailnet), 0o600); err != nil {
		t.Fatal(err)
	}
	p := freePorts(t, 4)
	cfg := config.Default()
	cfg.Node.DataDir = filepath.Join(home, "data")
	cfg.Listen.Raft, cfg.Listen.RPC, cfg.Listen.Web = p[0], p[1], p[2]
	cfg.Listen.NFS = fmt.Sprintf("127.0.0.1:%d", p[3])
	cfg.Mount.Path = filepath.Join(home, "mnt")
	cfg.Dev.Enabled, cfg.Dev.TailscaleFake = true, fake
	path := filepath.Join(home, "config.yaml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path, cfg
}

// run executes a CLI command in-process, as main wires it.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "tailraid", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("config", "", "")
	root.AddCommand(initCmd(), joinCmd(), nodeCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// openFSM reopens an initialised data directory to inspect what init wrote.
func openFSM(t *testing.T, cfg config.Config) *cluster.Node {
	t.Helper()
	id, err := cluster.LoadOrCreateNodeID(cfg.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := cluster.Open(cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, ClusterID: cid, RaftAddr: fmt.Sprintf("127.0.0.1:%d", cfg.Listen.Raft)})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	return n
}

// Invariant: I-76
func TestCLI_Init_NoFlags(t *testing.T) {
	path, cfg := devHome(t)
	out, err := run(t, "init", "--config", path, "--token", "testtoken")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "policy 1+0 (auto") || !strings.Contains(out, "budget ") {
		t.Fatalf("init output: %s", out)
	}
	host, _ := os.Hostname()
	n := openFSM(t, cfg)
	defer n.Close()
	vol := n.FSM.Volume()
	if !vol.Auto || vol.K != 1 || vol.M != 0 || vol.ChunkSize != 1<<20 {
		t.Fatalf("volume %+v", vol)
	}
	nodes := n.FSM.Nodes()
	if len(nodes) != 1 || nodes[0].Name != host || !nodes[0].Voter || nodes[0].Capacity == 0 {
		t.Fatalf("node table %+v (hostname %q)", nodes, host)
	}
	// the config written back keeps the budget derived
	got, err := config.Load(path)
	if err != nil || got.Node.Capacity != "" {
		t.Fatalf("saved config capacity %q %v", got.Node.Capacity, err)
	}
	// hidden --k/--m pin a manual policy for test rigs
	path2, cfg2 := devHome(t)
	if out, err := run(t, "init", "--config", path2, "--k", "1", "--m", "0"); err != nil || !strings.Contains(out, "(manual)") {
		t.Fatalf("manual init: %v\n%s", err, out)
	}
	n2 := openFSM(t, cfg2)
	defer n2.Close()
	if n2.FSM.Volume().Auto {
		t.Fatal("--k/--m must pin a manual policy")
	}
}

// Invariant: I-76
func TestCLI_Join_NoFlags(t *testing.T) {
	pathA, cfgA := devHome(t)
	if out, err := run(t, "init", "--config", pathA, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- node.Serve(ctx, cfgA, slog.Default(), "test") }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	addrA := fmt.Sprintf("127.0.0.1:%d", cfgA.Listen.RPC)
	probe := rpc.NewClient([16]byte{}, nil, nil)
	testharness.Eventually(t, func() bool { _, err := probe.Health(ctx, addrA); return err == nil }, "node A never served")

	pathB, _ := devHome(t)
	out, err := run(t, "join", "--config", pathB, addrA, "testtoken")
	if err != nil {
		t.Fatalf("join: %v\n%s", err, out)
	}
	if !strings.Contains(out, "joined cluster") {
		t.Fatalf("join output: %s", out)
	}
	cid, err := cluster.LoadClusterID(cfgA.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := admin.FetchStatus(ctx, rpc.NewClient(cid, nil, nil), addrA)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Nodes) != 2 {
		t.Fatalf("members %+v", rep.Nodes)
	}
	host, _ := os.Hostname()
	joined := rep.Nodes[1]
	if joined.Voter || joined.Capacity == 0 || joined.Name != host {
		t.Fatalf("joined node %+v", joined)
	}
	if !rep.Volume.Auto {
		t.Fatal("status does not report auto mode")
	}
}
