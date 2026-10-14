package mesh

import (
	"context"
	"strings"
	"testing"
)

// Invariant: I-324
func TestCreateTUN_NeedsAPrivilege(t *testing.T) {
	// Without CAP_NET_ADMIN, root or wintun the call fails; with them it must hand back a
	// named device. Either way the per-OS path links and closes cleanly; the VM rig is
	// where it runs for real (docs/vms.md).
	dev, name, err := CreateTUN("tailraid0", MTU, testCluster)
	if err != nil {
		t.Logf("unprivileged: %v", err)
		return
	}
	defer dev.Close()
	if name == "" {
		t.Fatal("a device with no name")
	}
	if mtu, err := dev.MTU(); err != nil || mtu != MTU {
		t.Fatalf("MTU %d %v, want %d", mtu, err, MTU)
	}
}

// Invariant: I-324
func TestExecRunner_ReportsTheCommand(t *testing.T) {
	err := ExecRunner(context.Background(), "tailraid-no-such-command-9f1", "x")
	if err == nil {
		t.Fatal("a missing command ran")
	}
	if got := err.Error(); !strings.HasPrefix(got, "tailraid-no-such-command-9f1 x") {
		t.Fatalf("the error does not name the command: %q", got)
	}
}
