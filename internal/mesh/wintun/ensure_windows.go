//go:build windows

package wintun

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureDLL writes the dll into dir when it is absent or differs from the embedded one,
// atomically, and reports whether it wrote (I-324). A matching file is left alone, so a
// running service keeps the dll it loaded.
func EnsureDLL(dir string) (written bool, err error) {
	path := filepath.Join(dir, Name)
	if cur, err := os.ReadFile(path); err == nil && sha256.Sum256(cur) == sha256.Sum256(dll) { //nolint:gosec // the dll beside our own binary
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, dll, 0o644); err != nil { //nolint:gosec // a dll the loader must read
		return false, fmt.Errorf("wintun: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("wintun: place %s: %w", path, err)
	}
	return true, nil
}

// embedded reports the dll's size, for the tests.
func embedded() int { return bytes.Count(dll, nil) - 1 }
