//go:build darwin

package admin

import (
	"encoding/binary"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// platformVitals uses sysctl: the load average, physical memory, the free page count and
// the interface list with its byte counters. macOS exposes no resident set or machine CPU
// counter to a pure-Go binary; those stay absent (I-85).
func platformVitals(m *MachineReport, tailnet *net.Interface) {
	rusageVitals(m)
	if raw, err := syscall.RouteRIB(syscall.NET_RT_IFLIST2, 0); err == nil { //nolint:staticcheck // the standard library's own sysctl of the route MIB; x/sys has no name for it
		idx := -1
		if tailnet != nil {
			idx = tailnet.Index
		}
		if rx, tx, trx, ttx, ok := sumIfList2(raw, idx); ok {
			m.NetRx, m.NetTx, m.TailnetRx, m.TailnetTx = rx, tx, trx, ttx
		}
	}
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }: three uint32, padding, int64
	if raw, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(raw) >= 24 {
		if scale := binary.LittleEndian.Uint64(raw[16:]); scale > 0 {
			m.Load1 = float64(binary.LittleEndian.Uint32(raw)) / float64(scale)
		}
	}
	if total, err := unix.SysctlUint64("hw.memsize"); err == nil {
		m.MemTotal = total
	}
	page, err := unix.SysctlUint32("hw.pagesize")
	if err != nil {
		return
	}
	var pages uint64
	for _, name := range []string{"vm.page_free_count", "vm.page_inactive_count", "vm.page_speculative_count"} {
		if n, err := unix.SysctlUint32(name); err == nil {
			pages += uint64(n)
		}
	}
	if pages > 0 {
		m.MemAvailable = pages * uint64(page)
	}
}

// The interface list is a run of routing messages; an RTM_IFINFO2 message is an
// if_msghdr2 whose if_data64 carries the byte counters. The fields are read at their
// offsets in x/sys/unix's IfMsghdr2 and IfData64 (checked by the test) rather than
// through an unsafe cast, since a message need not be 8-byte aligned.
const (
	ifm2Type  = 3  // Type uint8
	ifm2Flags = 8  // Flags int32
	ifm2Index = 12 // Index uint16
	ifm2Data  = 32 // Data IfData64
	ifd64In   = 64 // IfData64.Ibytes
	ifd64Out  = 72 // IfData64.Obytes
)

// sumIfList2 sums the byte counters of every interface but loopback and picks out the
// one at tailnet (an interface index; -1 for none).
func sumIfList2(b []byte, tailnet int) (rx, tx, trx, ttx uint64, ok bool) {
	const need = ifm2Data + ifd64Out + 8
	for off := 0; off+4 <= len(b); {
		ml := int(binary.LittleEndian.Uint16(b[off:]))
		if ml < 4 || off+ml > len(b) {
			break
		}
		if b[off+ifm2Type] == unix.RTM_IFINFO2 && ml >= need {
			flags := int32(binary.LittleEndian.Uint32(b[off+ifm2Flags:])) //nolint:gosec // the kernel's int32
			if flags&unix.IFF_LOOPBACK == 0 {
				in, out := binary.LittleEndian.Uint64(b[off+ifm2Data+ifd64In:]), binary.LittleEndian.Uint64(b[off+ifm2Data+ifd64Out:])
				rx, tx, ok = rx+in, tx+out, true
				if int(binary.LittleEndian.Uint16(b[off+ifm2Index:])) == tailnet {
					trx, ttx = in, out
				}
			}
		}
		off += ml
	}
	return rx, tx, trx, ttx, ok
}
