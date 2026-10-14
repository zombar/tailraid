package mesh

import (
	"net/netip"

	"golang.zx2c4.com/wireguard/tun"
)

// selfLoop hands back to the kernel every packet the OS gives the tunnel for the
// device's own address (docs/20 §1, I-317). macOS routes a host's traffic to its own
// utun address into the tunnel (the point-to-point route's destination is the address
// itself), where WireGuard has no peer for it, so a node could not reach its own
// listeners: the CLI's status, the web UI, the NFS gateway's client. Linux delivers such
// packets locally and never hands them to the TUN, so the loop sees none there.
type selfLoop struct {
	tun.Device
	addr netip.Addr
}

func (s *selfLoop) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := s.Device.Read(bufs, sizes, offset)
	kept := 0
	for i := range n {
		if isFor(bufs[i][offset:offset+sizes[i]], s.addr) {
			_, _ = s.Write([][]byte{bufs[i][:offset+sizes[i]]}, offset) // back to the kernel
			continue
		}
		if kept != i {
			bufs[kept], bufs[i] = bufs[i], bufs[kept]
			sizes[kept] = sizes[i]
		}
		kept++
	}
	return kept, err
}

// isFor reports whether pkt is an IPv4 packet whose destination is addr.
func isFor(pkt []byte, addr netip.Addr) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 || !addr.Is4() {
		return false
	}
	return netip.AddrFrom4([4]byte(pkt[16:20])) == addr
}
