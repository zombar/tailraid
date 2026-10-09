//go:build windows

package admin

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformVitals uses the process times and the global memory status; Windows exposes
// no load average to this binary, so it stays absent (I-85).
func platformVitals(m *MachineReport) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err == nil {
		m.CPUSeconds = (float64(kernel.Nanoseconds()) + float64(user.Nanoseconds())) / 1e9
	}
	type memoryStatusEx struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	if ok, _, _ := proc.Call(uintptr(unsafe.Pointer(&ms))); ok != 0 { //nolint:gosec // the struct outlives the call
		m.MemTotal, m.MemAvailable = ms.TotalPhys, ms.AvailPhys
	}
}
