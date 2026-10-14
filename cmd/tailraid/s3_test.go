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

// `node add-s3` takes the bucket's credentials from the secrets store (set first with
// `secret set`, the one way in), refuses a bucket they do not open before anything is
// registered, registers it once, and writes no file; the node serves the bucket from the
// store; no reply or report carries the secret (docs/10 §4.1, docs/19 §6).
// Invariant: I-137, I-139, I-308
func TestCLI_AddS3(t *testing.T) {
	path, cfg := devHome(t)
	cfg.Node.Disks = []config.Disk{{Path: filepath.Join(filepath.Dir(path), "d1"), Capacity: "1G"}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "init", "--config", path, "--token", "testtoken"); err != nil {
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
	add := func(name, bucket string) (string, error) {
		return run(t, "node", "add-s3", name, "--endpoint", srv.URL, "--bucket", bucket, "--prefix", "c1", "--path-style",
			"--capacity", "1T", "--config", path)
	}
	set := func(name, value string) {
		t.Helper()
		if out, err := runStdin(t, value+"\n", "secret", "set", name, "--config", path); err != nil {
			t.Fatalf("secret set %s: %v\n%s", name, err, out)
		}
	}
	// no credentials in the store: refused, naming the two secrets to set
	if out, err := add("offsite", s3test.Name); err == nil || !strings.Contains(err.Error(), "secret set s3/offsite/access_key") {
		t.Fatalf("without credentials: %v\n%s", err, out)
	}
	if out, err := add("Offsite", s3test.Name); err == nil || !strings.Contains(err.Error(), "lower-case") {
		t.Fatalf("a name off the secret pattern: %v\n%s", err, out)
	}
	// credentials the bucket refuses register nothing
	set("s3/offsite/access_key", s3test.AccessKey)
	set("s3/offsite/secret_key", "wrong")
	if out, err := add("offsite", s3test.Name); err == nil || !strings.Contains(err.Error(), "nothing was registered") {
		t.Fatalf("wrong secret: %v\n%s", err, out)
	}
	set("s3/offsite/secret_key", s3test.SecretKey)
	out, err := add("offsite", s3test.Name)
	if err != nil || !strings.Contains(out, "added S3 node offsite") {
		t.Fatalf("add-s3: %v\n%s", err, out)
	}
	if keys := srv.Keys(); len(keys) != 0 {
		t.Fatalf("the bucket check left %v", keys)
	}
	if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, cluster.S3KeysFile)); err == nil {
		t.Fatal("add-s3 wrote the old keys file")
	}
	// the same bucket again, under any name or prefix, is refused by the leader
	set("s3/second/access_key", s3test.AccessKey)
	set("s3/second/secret_key", s3test.SecretKey)
	if out, err := add("second", s3test.Name); err == nil {
		t.Fatalf("second row for one bucket: %s", out)
	}
	ls, err := run(t, "secret", "ls", "--json", "--config", path)
	if err != nil || !strings.Contains(ls, `"s3/offsite/secret_key"`) || strings.Contains(ls, s3test.SecretKey) {
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
}
