//go:build windows

package wintun

import (
	"os"
	"path/filepath"
	"testing"
)

// The dll lands beside the binary once, is left alone when it matches, and is replaced
// when it differs.
// Invariant: I-324
func TestEnsureDLL_WritesOnceAndReplaces(t *testing.T) {
	dir := t.TempDir()
	written, err := EnsureDLL(dir)
	if err != nil || !written {
		t.Fatalf("first: %v %v", written, err)
	}
	st, err := os.Stat(filepath.Join(dir, Name))
	if err != nil || st.Size() != int64(embedded()) {
		t.Fatalf("written dll: %v %v", st, err)
	}
	if written, err := EnsureDLL(dir); err != nil || written {
		t.Fatalf("second: %v %v (a matching dll is left alone)", written, err)
	}
	if err := os.WriteFile(filepath.Join(dir, Name), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if written, err := EnsureDLL(dir); err != nil || !written {
		t.Fatalf("stale: %v %v", written, err)
	}
	if _, err := os.Stat(filepath.Join(dir, Name+".tmp")); err == nil {
		t.Fatal("the temp file was left behind")
	}
}
