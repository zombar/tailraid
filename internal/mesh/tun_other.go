//go:build !windows

package mesh

import (
	"fmt"
	"runtime"

	"golang.zx2c4.com/wireguard/tun"
)

// CreateTUN opens the interface: the named one on Linux, the next free utun on macOS.
// The returned name is what the OS calls it. clusterID is used on Windows only.
func CreateTUN(name string, mtu int, _ [16]byte) (tun.Device, string, error) {
	if runtime.GOOS == "darwin" {
		name = "utun"
	}
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, "", fmt.Errorf("mesh: create %s: %w (docs/20 §9: the interface needs a privilege)", name, err)
	}
	actual, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, "", fmt.Errorf("mesh: interface name: %w", err)
	}
	return dev, actual, nil
}
