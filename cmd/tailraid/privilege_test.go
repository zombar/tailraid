package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
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
