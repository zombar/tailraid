package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/config"
)

// status --json on a device carries machine.paths (the data directory and each disk with
// its encryption answer) and repeats serve's files-on-disk warnings: a disk left 0755
// after init is a chmod 700 line (docs/08 §Files on disk).
// Invariant: I-341
func TestStatus_CarriesPathsAndWarnings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	path, cfg := devHome(t)
	disk := filepath.Join(filepath.Dir(path), "disk1")
	cfg.Node.Disks = []config.Disk{{Path: disk, Capacity: "1G"}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "init", "--config", path, "--token", "testtoken"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if err := os.Chmod(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	serveInBackground(t, cfg)
	out, err := run(t, "status", "--config", path, "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var rep admin.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status json: %v\n%s", err, out)
	}
	if rep.Machine == nil || len(rep.Machine.Paths) != 2 {
		t.Fatalf("machine.paths: %+v", rep.Machine)
	}
	for i, want := range []config.PathEntry{{Kind: "data directory", Path: cfg.Node.DataDir}, {Kind: "disk", Path: disk}} {
		p := rep.Machine.Paths[i]
		if p.Kind != want.Kind || p.Path != want.Path {
			t.Fatalf("path %d: %+v, want %+v", i, p, want)
		}
		switch p.Encrypted {
		case admin.EncryptedOn, admin.EncryptedOff, admin.EncryptedUnknown:
		default:
			t.Fatalf("path %d: encrypted %q", i, p.Encrypted)
		}
	}
	var chmod bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "disk "+disk) && strings.Contains(w, "chmod 700") {
			chmod = true
		}
	}
	if !chmod {
		t.Fatalf("no chmod warning for the 0755 disk: %v", rep.Warnings)
	}
}
