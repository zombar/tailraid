//go:build darwin

package admin

import (
	"context"
	"os/exec"
	"time"
)

// platformEncryption asks diskutil about the volume that holds path.
func platformEncryption(path string) Encryption {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "diskutil", "info", path).Output() //nolint:gosec // the path is the config's own; no shell
	if err != nil {
		return EncryptedUnknown
	}
	return parseDiskutil(string(out))
}
