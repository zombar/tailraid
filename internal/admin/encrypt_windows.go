//go:build windows

package admin

import (
	"context"
	"os/exec"
	"path/filepath"
	"time"
)

// platformEncryption asks manage-bde about the drive that holds path; without the right
// to ask (the tool needs an elevated caller) the answer is unknown.
func platformEncryption(path string) Encryption {
	abs, err := filepath.Abs(path)
	if err != nil {
		return EncryptedUnknown
	}
	drive := filepath.VolumeName(abs)
	if drive == "" {
		return EncryptedUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "manage-bde", "-status", drive).Output() //nolint:gosec // the drive letter of the config's own path; no shell
	if err != nil {
		return EncryptedUnknown
	}
	return parseManageBDE(string(out))
}
