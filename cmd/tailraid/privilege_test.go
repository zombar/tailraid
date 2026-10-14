package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
)

// A command run under sudo hands its files back to the user on Linux and records that
// user as the mount's owner on macOS; without sudo neither does anything.
// Invariant: I-324
func TestPrivilege_GiveBackAndSudoOwner(t *testing.T) {
	t.Setenv("SUDO_UID", "")
	t.Setenv("SUDO_GID", "")
	if got := sudoOwner(); got != "" {
		t.Fatalf("without sudo: %q", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := giveBack(dir, filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("without sudo: %v", err)
	}
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()))
	t.Setenv("SUDO_GID", strconv.Itoa(os.Getgid()))
	if got := sudoOwner(); runtime.GOOS != "windows" && os.Getuid() != 0 && got != strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()) {
		t.Fatalf("under sudo: %q", got)
	}
	if err := giveBack(dir); err != nil { // to the same user on Linux, nothing elsewhere
		t.Fatalf("give back to self: %v", err)
	}
	t.Setenv("SUDO_UID", "0")
	if got := sudoOwner(); got != "" {
		t.Fatalf("sudo from root: %q", got)
	}
}

// What a sudo init or join gives back: the data directory, the config's directory and
// every disk and mirror directory the config lists, in that order; none twice.
// Invariant: I-324
func TestPrivilege_OwnedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a sudo join is Linux and macOS: the paths are POSIX ones, which filepath.Dir would split on a backslash here")
	}
	var cfg config.Config
	cfg.Node.DataDir = "/home/u/.config/tailraid/data"
	cfg.Node.Disks = []config.Disk{{Path: "/home/u/tailraid-d1"}, {Path: "/mnt/big"}}
	cfg.Node.Mirrors = []config.Mirror{{Path: "/mnt/archive"}}
	got := ownedPaths(cfg, "/home/u/.config/tailraid/config.yaml")
	want := []string{"/home/u/.config/tailraid/data", "/home/u/.config/tailraid", "/home/u/tailraid-d1", "/mnt/big", "/mnt/archive"}
	if !slices.Equal(got, want) {
		t.Fatalf("owned paths %v, want %v", got, want)
	}
	if got := ownedPaths(config.Config{}, "/x/config.yaml"); !slices.Equal(got, []string{"", "/x"}) {
		t.Fatalf("no disks: %v", got)
	}
}

// The cluster ID comes from the identity file when it is readable and from the node's
// own /health otherwise: a user's CLI beside the root daemon on macOS reads no file of
// the data directory, and a node that answers nothing is "not initialised".
// Invariant: I-324
func TestClusterIDOf_FileThenHealth(t *testing.T) {
	want := [16]byte{0xab, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_id":"n","cluster_id":"` + hex.EncodeToString(want[:]) + `","version":"v","applied":1,"codec":1,"ready":true}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	var cfg config.Config
	cfg.Node.DataDir = filepath.Join(t.TempDir(), "unreadable-or-absent")
	got, err := clusterIDOf(context.Background(), cfg, addr, netip.MustParseAddr("127.0.0.1"))
	if err != nil || got != want {
		t.Fatalf("from health: %x %v", got, err)
	}
	cfg.Node.DataDir = t.TempDir()
	onDisk := [16]byte{9}
	if err := cluster.WriteClusterID(cfg.Node.DataDir, hex.EncodeToString(onDisk[:])); err != nil {
		t.Fatal(err)
	}
	if got, err := clusterIDOf(context.Background(), cfg, addr, netip.Addr{}); err != nil || got != onDisk {
		t.Fatalf("from the file: %x %v", got, err)
	}
	cfg.Node.DataDir = filepath.Join(t.TempDir(), "absent")
	if _, err := clusterIDOf(context.Background(), cfg, "127.0.0.1:1", netip.Addr{}); err == nil || !strings.Contains(err.Error(), "not initialised") {
		t.Fatalf("nothing answers: %v", err)
	}
	if got, err := clusterIDFrom(admin.Report{ClusterID: hex.EncodeToString(want[:])}); err != nil || got != want {
		t.Fatalf("from a report: %x %v", got, err)
	}
	if _, err := clusterIDFrom(admin.Report{ClusterID: "zz"}); err == nil {
		t.Fatal("a report without a cluster id gave one")
	}
}
