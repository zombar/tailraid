//go:build !windows

package wintun

import "testing"

// Invariant: I-324
func TestEnsureDLL_NothingOffWindows(t *testing.T) {
	if written, err := EnsureDLL(t.TempDir()); written || err != nil {
		t.Fatalf("off Windows: %v %v", written, err)
	}
}
