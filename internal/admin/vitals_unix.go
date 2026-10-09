//go:build unix

package admin

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// rusageVitals fills the process fields every unix exposes: CPU time and the peak
// resident set (in bytes on macOS, KiB elsewhere).
func rusageVitals(m *MachineReport) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return
	}
	m.CPUSeconds = float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
	peak := uint64(max(ru.Maxrss, 0))
	if runtime.GOOS != "darwin" {
		peak <<= 10
	}
	m.RSSPeak = peak
}
