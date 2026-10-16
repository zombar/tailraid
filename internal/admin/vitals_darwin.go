//go:build darwin

package admin

import (
	"encoding/binary"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// platformVitals uses sysctl: the load average, physical memory, the free page count and
// the interface list with its byte counters. macOS exposes no resident set or machine CPU
// counter to a pure-Go binary; those stay absent (I-85).
func platformVitals(m *MachineReport, tailnet *net.Interface) {
	rusageVitals(m)
	procNet.apply(m, func() []ifCount {
		raw, err := syscall.RouteRIB(syscall.NET_RT_IFLIST2, 0) //nolint:staticcheck // the standard library's own sysctl of the route MIB; x/sys has no name for it
		if err != nil {
			return nil
		}
		idx := -1
		if tailnet != nil {
			idx = tailnet.Index
		}
		return ifList2(raw, idx)
	})
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

// ifList2 reads the byte counters of every interface but loopback, marking the one at
// tailnet (an interface index; -1 for none).
func ifList2(b []byte, tailnet int) []ifCount {
	const need = ifm2Data + ifd64Out + 8
	var ifs []ifCount
	for off := 0; off+4 <= len(b); {
		ml := int(binary.LittleEndian.Uint16(b[off:]))
		if ml < 4 || off+ml > len(b) {
			break
		}
		if b[off+ifm2Type] == unix.RTM_IFINFO2 && ml >= need {
			flags := int32(binary.LittleEndian.Uint32(b[off+ifm2Flags:])) //nolint:gosec // the kernel's int32
			if flags&unix.IFF_LOOPBACK == 0 {
				idx := int(binary.LittleEndian.Uint16(b[off+ifm2Index:]))
				ifs = append(ifs, ifCount{
					ID: strconv.Itoa(idx), Rx: binary.LittleEndian.Uint64(b[off+ifm2Data+ifd64In:]), Tx: binary.LittleEndian.Uint64(b[off+ifm2Data+ifd64Out:]),
					Tailnet: idx == tailnet,
				})
			}
		}
		off += ml
	}
	return ifs
}
