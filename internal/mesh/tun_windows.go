//go:build windows

package mesh

import (
	"fmt"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/tun"
)

// CreateTUN opens the wintun adapter named name, with a GUID derived from the cluster ID
// so the same adapter is reused across restarts (docs/20 §1).
func CreateTUN(name string, mtu int, clusterID [16]byte) (tun.Device, string, error) {
	guid := windows.GUID{
		Data1: uint32(clusterID[0])<<24 | uint32(clusterID[1])<<16 | uint32(clusterID[2])<<8 | uint32(clusterID[3]),
		Data2: uint16(clusterID[4])<<8 | uint16(clusterID[5]),
		Data3: uint16(clusterID[6])<<8 | uint16(clusterID[7]),
	}
	copy(guid.Data4[:], clusterID[8:16])
	tun.WintunTunnelType = "tailraid"
	dev, err := tun.CreateTUNWithRequestedGUID(name, &guid, mtu)
	if err != nil {
		return nil, "", fmt.Errorf("mesh: create %s: %w (docs/20 §9: wintun.dll beside the exe, and the service runs as LocalSystem)", name, err)
	}
	actual, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, "", fmt.Errorf("mesh: interface name: %w", err)
	}
	return dev, actual, nil
}
