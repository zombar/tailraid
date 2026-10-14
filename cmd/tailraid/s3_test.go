package main

import (
	"context"
	"encoding/json"
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
	"github.com/zombar/tailraid/internal/s3/s3test"
	"github.com/zombar/tailraid/internal/testharness"
)

// `node add-s3` checks the bucket, registers it once and stores the key in the secrets
// store, never in a file, a report or in plaintext in the FSM; the node serves the bucket
// from the store; `node keys` stores a key only after the bucket accepts it (docs/10
// §4.1, docs/19 §6).
// Invariant: I-137, I-139, I-308
func TestCLI_AddS3_AndKeys(t *testing.T) {
	path, cfg := devHome(t)
	cfg.Node.Disks = []config.Disk{{Path: filepath.Join(filepath.Dir(path), "d1"), Capacity: "1G"}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "init", "--config", path, "--token", testToken); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
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
	waitReady(ctx, t, addr, "node never became ready")
	srv := s3test.New()
	defer srv.Close()
	add := func(name, bucket, secret string) (string, error) {
		t.Setenv(SecretKeyEnv, secret)
		return run(t, "node", "add-s3", name, "--endpoint", srv.URL, "--bucket", bucket, "--prefix", "c1", "--path-style",
			"--capacity", "1T", "--access-key", s3test.AccessKey, "--config", path)
	}
	// a key the bucket refuses registers nothing and saves nothing
	if out, err := add("offsite", s3test.Name, "wrong"); err == nil || !strings.Contains(err.Error(), "nothing was registered") {
		t.Fatalf("wrong secret: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, cluster.S3KeysFile)); err == nil {
		t.Fatal("a refused add saved a key")
	}
	out, err := add("offsite", s3test.Name, s3test.SecretKey)
	if err != nil || !strings.Contains(out, "added S3 node offsite") {
		t.Fatalf("add-s3: %v\n%s", err, out)
	}
	if keys := srv.Keys(); len(keys) != 0 {
		t.Fatalf("the bucket check left %v", keys)
	}
	// the same bucket again, under any name or prefix, is refused by the leader
	if out, err := add("second", s3test.Name, s3test.SecretKey); err == nil {
		t.Fatalf("second row for one bucket: %s", out)
	}
	if !strings.Contains(out, "secrets store") {
		t.Fatalf("add-s3 did not store the key: %s", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, cluster.S3KeysFile)); err == nil {
		t.Fatal("add-s3 wrote the old keys file")
	}
	ls, err := run(t, "secret", "ls", "--json", "--config", path)
	if err != nil || !strings.Contains(ls, `"s3/offsite"`) || strings.Contains(ls, s3test.SecretKey) {
		t.Fatalf("secret ls: %v\n%s", err, ls)
	}
	cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	c := rpc.NewClient(cid, nil)
	var rep admin.Report
	testharness.Eventually(t, func() bool {
		rep, err = admin.FetchStatus(ctx, c, addr)
		if err != nil {
			return false
		}
		for _, n := range rep.Nodes {
			if n.Kind == "s3" && n.Live {
				return true
			}
		}
		return false
	}, "the bucket live in status")
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), s3test.SecretKey) || !strings.Contains(string(raw), srv.URL) {
		t.Fatalf("status names the secret, or not the endpoint: %s", raw)
	}
	// node keys: refused for a wrong secret, saved for the right one
	t.Setenv(SecretKeyEnv, "wrong")
	if out, err := run(t, "node", "keys", "offsite", "--access-key", s3test.AccessKey, "--config", path); err == nil {
		t.Fatalf("keys with a wrong secret: %s", out)
	}
	t.Setenv(SecretKeyEnv, "")
	root := []string{"node", "keys", "offsite", "--access-key", s3test.AccessKey, "--config", path}
	if out, err := runStdin(t, s3test.SecretKey+"\n", root...); err != nil || !strings.Contains(out, "stored the key of offsite") {
		t.Fatalf("keys from stdin: %v\n%s", err, out)
	}
}
