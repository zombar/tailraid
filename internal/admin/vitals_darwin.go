//go:build darwin

package admin

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// platformVitals uses sysctl: the load average, physical memory and the free page count.
// macOS exposes no resident set or machine CPU counter to a pure-Go binary; those stay
// absent (I-85).
func platformVitals(m *MachineReport) {
	rusageVitals(m)
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
