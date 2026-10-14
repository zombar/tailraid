package mesh

import (
	"errors"
	"fmt"
	"net/netip"
)

// The cluster subnet is a /24 (docs/20 §3): hosts 1..MaxHost, the first for init's node.
const (
	SubnetBits = 24
	FirstHost  = 1
	MaxHost    = 254
	// MTU is the inner MTU: the IPv6 minimum, so a relayed packet inside an IPv6 outer
	// still fits a PPPoE or mobile path (docs/20 §1).
	MTU = 1280
	// DefaultPort is the UDP port of the device.
	DefaultPort = 20483
)

// ErrHost is a host number outside the subnet.
var ErrHost = errors.New("mesh: host number out of range")

// Subnet derives the cluster's /24 inside 100.64.0.0/10 from its ID: one of 16,384, the
// same on every node (I-320).
func Subnet(clusterID [16]byte) netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64 + clusterID[0]%64, clusterID[1], 0}), SubnetBits)
}

// HostAddr is host n of the subnet.
func HostAddr(subnet netip.Prefix, n uint8) (netip.Addr, error) {
	if n < FirstHost || n > MaxHost {
		return netip.Addr{}, fmt.Errorf("%w: %d", ErrHost, n)
	}
	b := subnet.Masked().Addr().As4()
	b[3] = n
	return netip.AddrFrom4(b), nil
}
