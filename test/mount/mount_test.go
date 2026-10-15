// Package mount_test mounts a real single-node gateway through the OS NFS client and
// runs a shell workload on it (docs/09 §real-mount integration). It needs root (or
// passwordless sudo) and is gated on TAILRAID_MOUNT_TEST=1:
//
//	sudo -E make test-mount
package mount_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	mnt "github.com/zombar/tailraid/internal/mount"
	"github.com/zombar/tailraid/internal/node"
)

func sh(t *testing.T, dir, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-ec", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s\n%s", err, script, out)
	}
	return string(out)
}

// Invariant: I-1, I-7, I-34
// Lifecycle: L-1, L-2, L-5, L-8, L-16
func TestMount_Workload(t *testing.T) {
	if os.Getenv("TAILRAID_MOUNT_TEST") != "1" {
		t.Skip("set TAILRAID_MOUNT_TEST=1 and run as root (sudo -E make test-mount)")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows mounts are validated manually (docs/windows-checklist.md)")
	}
	home := t.TempDir()
	cfg := config.Default()
	cfg.Node.DataDir = filepath.Join(home, "data")
	cfg.Node.Disks = []config.Disk{{Path: filepath.Join(home, "disk"), Capacity: "20G"}} // the volume's one disk (docs/14 §9)
	cfg.Listen.NFS = "127.0.0.1:20491"
	cfg.Listen.Raft = 20482
	cfg.Listen.RPC = 20483
	cfg.Listen.Web = 20484 // the default web port is this test's Raft port
	cfg.Mount.Path = filepath.Join(home, "mnt")
	// no tailscale CLI on CI runners: the dev fake makes loopback this node's tailnet IP
	fake := filepath.Join(home, "ts.json")
	if err := os.WriteFile(fake, []byte(`{"status":{"BackendState":"Running","TailscaleIPs":["127.0.0.1"],
		"Self":{"ID":"ts-mounttest","HostName":"mounttest","TailscaleIPs":["127.0.0.1"],"Online":true},"Peer":{}},
		"whois":{"127.0.0.1":{"Node":{"StableID":"ts-mounttest","Name":"mounttest.","Addresses":["127.0.0.1/32"]}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Dev.Enabled, cfg.Dev.TailscaleFake = true, fake
	id, err := cluster.LoadOrCreateNodeID(cfg.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := cluster.Bootstrap(context.Background(), cluster.Options{DataDir: cfg.Node.DataDir, NodeID: id, RaftAddr: "127.0.0.1:20482"},
		meta.Node{Name: "mounttest", Kind: meta.KindDevice, Voter: true},
		meta.SetVolume{ChunkSize: 1 << 20, K: 1, M: 0, Now: time.Now().UnixNano()}, false, "t")
	if err != nil {
		t.Fatal(err)
	}
	cid, err := cluster.LoadClusterID(cfg.Node.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.RegisterDisks(context.Background(), n, cfg, cid); err != nil {
		t.Fatal(err)
	}
	_ = n.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- node.Serve(ctx, cfg, slog.Default(), "test") }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	waitListening(t, cfg.Listen.NFS)
	if err := os.MkdirAll(cfg.Mount.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	prog, args, err := mnt.Command(runtime.GOOS, cfg.Listen.NFS, cfg.Mount.Path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := mnt.Run(ctx, &out, runtime.GOOS, prog, args); err != nil {
		t.Fatalf("mount: %v\n%s", err, out.String())
	}
	t.Cleanup(func() {
		uprog, uargs, _ := mnt.UnmountCommand(runtime.GOOS, cfg.Mount.Path)
		_ = mnt.Run(context.Background(), &out, runtime.GOOS, uprog, uargs)
	})
	m := cfg.Mount.Path
	src := filepath.Join(home, "src")
	sh(t, home, fmt.Sprintf(`
mkdir -p %[1]s/tree/a/b %[1]s/tree/c
head -c 3000000 /dev/urandom > %[1]s/tree/a/big.bin
head -c 777 /dev/urandom > %[1]s/tree/a/b/small.bin
printf 'hello\n' > %[1]s/tree/c/hello.txt
for i in $(seq 1 200); do printf 'file %%s\n' $i > %[1]s/tree/c/f$i.txt; done
cp -R %[1]s/tree %[2]s/tree
diff -r %[1]s/tree %[2]s/tree
`, src, m))
	// random-access writes at offsets, compared against the same ops on a local file
	sh(t, home, fmt.Sprintf(`
head -c 8388608 /dev/urandom > %[1]s/ref.bin
cp %[1]s/ref.bin %[2]s/rw.bin
head -c 70000 /dev/urandom > %[1]s/patch.bin
dd if=%[1]s/patch.bin of=%[2]s/rw.bin bs=1 seek=3000000 conv=notrunc 2>/dev/null
dd if=%[1]s/patch.bin of=%[1]s/ref.bin bs=1 seek=3000000 conv=notrunc 2>/dev/null
dd if=%[1]s/patch.bin of=%[2]s/rw.bin bs=1 seek=8388600 conv=notrunc 2>/dev/null
dd if=%[1]s/patch.bin of=%[1]s/ref.bin bs=1 seek=8388600 conv=notrunc 2>/dev/null
cmp %[1]s/ref.bin %[2]s/rw.bin
truncate -s 5000000 %[2]s/rw.bin 2>/dev/null || python3 -c 'import os; os.truncate("%[2]s/rw.bin", 5000000)'
truncate -s 5000000 %[1]s/ref.bin 2>/dev/null || python3 -c 'import os; os.truncate("%[1]s/ref.bin", 5000000)'
cmp %[1]s/ref.bin %[2]s/rw.bin
`, src, m))
	// rsync-style write-tmp-then-rename, 50 times
	sh(t, m, `
for i in $(seq 1 50); do printf 'v%s\n' $i > tmp.$$; mv tmp.$$ versioned.txt; done
test "$(cat versioned.txt)" = "v50"
`)
	// idle flush, then compare after the flusher has committed everything
	time.Sleep(4 * time.Second) //nolint:forbidigo // real idle timer of the running node
	sh(t, home, fmt.Sprintf(`diff -r %[1]s/tree %[2]s/tree && cmp %[1]s/ref.bin %[2]s/rw.bin`, src, m))
	// unmount, remount, compare again (handles and data survive)
	uprog, uargs, _ := mnt.UnmountCommand(runtime.GOOS, m)
	if err := mnt.Run(ctx, &out, runtime.GOOS, uprog, uargs); err != nil {
		t.Fatalf("umount: %v", err)
	}
	if err := mnt.Run(ctx, &out, runtime.GOOS, prog, args); err != nil {
		t.Fatalf("remount: %v", err)
	}
	sh(t, home, fmt.Sprintf(`diff -r %[1]s/tree %[2]s/tree && cmp %[1]s/ref.bin %[2]s/rw.bin`, src, m))
	// cross-check one file's hash through the raw filesystem view
	want := sha256.Sum256([]byte(sh(t, home, fmt.Sprintf("cat %s/tree/a/big.bin", src))))
	got := sha256.Sum256([]byte(sh(t, home, fmt.Sprintf("cat %s/tree/a/big.bin", m))))
	if want != got {
		t.Fatal("big.bin hash differs through the mount")
	}
}

// waitListening polls the gateway port until it accepts connections.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		<-time.After(100 * time.Millisecond)
	}
	t.Fatalf("gateway %s never came up", addr)
}
