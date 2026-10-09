package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/config"
)

// volume grow rewrites exactly the named disk's capacity (by name, number or path), auto
// clears it, and an unknown disk or a bad size leaves the config untouched.
//
// Invariant: I-130
func TestCLI_VolumeGrow_EditsConfig(t *testing.T) {
	path, cfg := devHome(t)
	home := filepath.Dir(path)
	host, _ := os.Hostname()
	cfg.Node.Name = host
	cfg.Node.Disks = []config.Disk{{Path: filepath.Join(home, "d1"), Capacity: "1G"}, {Path: filepath.Join(home, "d2")}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	load := func() config.Config {
		t.Helper()
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	out, err := run(t, "volume", "grow", "--config", path, host+"/2", "50G", "--no-restart")
	if err != nil || !strings.Contains(out, host+"/2: budget pinned at 50G") || !strings.Contains(out, "restart the service") {
		t.Fatalf("grow by name: %v\n%s", err, out)
	}
	if c := load(); c.Node.Disks[1].Capacity != "50G" || c.Node.Disks[0].Capacity != "1G" {
		t.Fatalf("config after grow by name: %+v", c.Node.Disks)
	}
	if _, err := run(t, "volume", "grow", "--config", path, "1", "auto", "--no-restart"); err != nil {
		t.Fatalf("grow by number: %v", err)
	}
	if c := load(); c.Node.Disks[0].Capacity != "" || c.Node.Disks[1].Capacity != "50G" {
		t.Fatalf("config after auto: %+v", c.Node.Disks)
	}
	if _, err := run(t, "volume", "grow", "--config", path, filepath.Join(home, "d1"), "2T", "--no-restart"); err != nil {
		t.Fatalf("grow by path: %v", err)
	}
	if c := load(); c.Node.Disks[0].Capacity != "2T" {
		t.Fatalf("config after grow by path: %+v", c.Node.Disks)
	}
	before, _ := os.ReadFile(path)
	for _, args := range [][]string{
		{"nope", "1G"},
		{"3", "1G"},
		{host + "/1", "lots"},
		{host + "/1"},
	} {
		if _, err := run(t, append([]string{"volume", "grow", "--config", path}, append(args, "--no-restart")...)...); err == nil {
			t.Fatalf("grow %v accepted", args)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
			t.Fatalf("grow %v changed the config", args)
		}
	}
	// no arguments: the explanation, nothing written
	out, err = run(t, "volume", "grow", "--config", path)
	if err != nil || !strings.Contains(out, "volume grow <disk> <size>") {
		t.Fatalf("explanation: %v\n%s", err, out)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Fatal("the explanation changed the config")
	}
	// a device without disks says so
	cfg.Node.Disks = nil
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "volume", "grow", "--config", path, "1", "1G", "--no-restart"); err == nil || !strings.Contains(err.Error(), "no disks") {
		t.Fatalf("no disks: %v", err)
	}
}
