//go:build unix

package config

import (
	"os"
	"testing"
)

// The owner of a path this process made is this process's uid; the answer is the OS's,
// so a path of another user would differ.
// Invariant: I-341
func TestOwnedBy(t *testing.T) {
	st, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := ownedBy(st)
	if !ok || uid != os.Getuid() {
		t.Fatalf("ownedBy = %d, %v; this process is uid %d", uid, ok, os.Getuid())
	}
}
