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
	for range n {
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
	p := freePorts(t, 5)
	cfg := config.Default()
	cfg.Node.DataDir = filepath.Join(home, "data")
	cfg.Listen.Raft, cfg.Listen.RPC, cfg.Listen.Web = p[0], p[1], p[2]
	cfg.Listen.NFS = fmt.Sprintf("127.0.0.1:%d", p[3])
	cfg.Listen.SMB = fmt.Sprintf("127.0.0.1:%d", p[4]) // both gateways, so serve runs both
	cfg.SMB.Password = config.NewSMBPassword()
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
	root.AddCommand(initCmd(), joinCmd(), nodeCmd(), volumeCmd())
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
	if !strings.Contains(out, "policy 1+0 (auto") || !strings.Contains(out, node.NoDisksHint) {
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
	if len(nodes) != 1 || nodes[0].Name != host || !nodes[0].Voter || nodes[0].Capacity != 0 {
		t.Fatalf("node table %+v (hostname %q): one device row, no budget", nodes, host)
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

// init generates the SMB share password when listen.smb is set and the config has none,
// saves it in the config and never prints it (docs/15 §6).
// Invariant: I-110
func TestCLI_Init_GeneratesSMBPassword(t *testing.T) {
	path, _ := devHome(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.Contains(line, "password:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "init", "--config", path, "--token", "testtoken")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SMB.Password) != 24 {
		t.Fatalf("saved password %q", got.SMB.Password)
	}
	if strings.Contains(out, got.SMB.Password) {
		t.Fatalf("init printed the password:\n%s", out)
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
	probe := rpc.NewClient([16]byte{}, nil)
	testharness.Eventually(t, func() bool { _, err := probe.Health(ctx, addrA); return err == nil }, "node A never served")

	pathB, _ := devHome(t)
	out, err := run(t, "join", "--config", pathB, addrA, "testtoken")
	if err != nil {
		t.Fatalf("join: %v\n%s", err, out)
	}
	if !strings.Contains(out, "joined cluster") {
		t.Fatalf("join output: %s", out)
	}
	// the members the response listed are persisted for serve (I-92)
	cfgB, err := config.Load(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if seed, err := cluster.LoadMemberSeed(cfgB.Node.DataDir); err != nil || len(seed) == 0 || seed[0].Addr != "127.0.0.1" {
		t.Fatalf("member seed after join: %+v %v", seed, err)
	}
	// and the log it received is snapshotted, so serve restores the table at open (the
	// snapshot waits for the FSM to pass the joiner's own configuration entry)
	if snaps, err := os.ReadDir(filepath.Join(cfgB.Node.DataDir, "raft", "snapshots")); err != nil || len(snaps) == 0 {
		t.Fatalf("no raft snapshot after the join: %v %v", snaps, err)
	}
	cid, err := cluster.LoadClusterID(cfgA.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := admin.FetchStatus(ctx, rpc.NewClient(cid, nil), addrA)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Nodes) != 2 {
		t.Fatalf("members %+v", rep.Nodes)
	}
	host, _ := os.Hostname()
	joined := rep.Nodes[1]
	if joined.Voter || joined.Capacity != 0 || joined.Name != host || !joined.Diskless {
		t.Fatalf("joined node %+v: a non-voter with no budget and no disks", joined)
	}
	if !rep.Volume.Auto {
		t.Fatal("status does not report auto mode")
	}
	// the joined node serves: a fresh follower has no snapshot, so its table is empty
	// until the leader connects, and the leader must be accepted before that (I-92)
	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan error, 1)
	go func() { doneB <- node.Serve(ctxB, cfgB, slog.Default(), "test") }()
	t.Cleanup(func() {
		cancelB()
		if err := <-doneB; err != nil {
			t.Errorf("serve B: %v", err)
		}
	})
	addrB := fmt.Sprintf("127.0.0.1:%d", cfgB.Listen.RPC)
	testharness.Eventually(t, func() bool {
		h, err := probe.Health(ctx, addrB)
		return err == nil && h.Ready
	}, "the joined node never became ready")
	testharness.Eventually(t, func() bool {
		rep, err := admin.FetchStatus(ctx, rpc.NewClient(cid, nil), addrA)
		return err == nil && len(rep.Nodes) == 2 && rep.Nodes[1].Live
	}, "the leader never saw the joined node live")
}

// A restarted follower whose leader is gone serves anyway: its health answers within
// seconds with ready=false, and it is ready once the leader is back. A fresh follower
// has a snapshot since the join took one, so this holds from the first restart on.
// Invariant: I-134, I-125
func TestCLI_Serve_WithoutLeader_StartsStale(t *testing.T) {
	pathA, cfgA := devHome(t)
	if out, err := run(t, "init", "--config", pathA, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	serve := func(cfg config.Config) func() { // returns the stop
		sctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- node.Serve(sctx, cfg, slog.Default(), "test") }()
		return func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
	}
	ctx := context.Background()
	stopA := serve(cfgA)
	addrA := fmt.Sprintf("127.0.0.1:%d", cfgA.Listen.RPC)
	probe := rpc.NewClient([16]byte{}, nil)
	testharness.Eventually(t, func() bool { _, err := probe.Health(ctx, addrA); return err == nil }, "node A never served")
	pathB, _ := devHome(t)
	if out, err := run(t, "join", "--config", pathB, addrA, "testtoken"); err != nil {
		t.Fatalf("join: %v\n%s", err, out)
	}
	cfgB, err := config.Load(pathB)
	if err != nil {
		t.Fatal(err)
	}
	addrB := fmt.Sprintf("127.0.0.1:%d", cfgB.Listen.RPC)
	stopB := serve(cfgB)
	testharness.Eventually(t, func() bool { h, err := probe.Health(ctx, addrB); return err == nil && h.Ready }, "B never became ready")
	stopB()
	stopA() // the leader is gone; B, a non-voter, cannot elect one
	stopB = serve(cfgB)
	defer stopB()
	start := time.Now()
	var h rpc.Health
	testharness.Eventually(t, func() bool { var err error; h, err = probe.Health(ctx, addrB); return err == nil }, "B did not serve without a leader")
	if h.Ready {
		t.Fatal("B reports ready without a leader")
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("B took %v to answer without a leader", took)
	}
	stopA = serve(cfgA)
	defer stopA()
	testharness.Eventually(t, func() bool { h, err := probe.Health(ctx, addrB); return err == nil && h.Ready }, "B not ready once the leader is back")
}
