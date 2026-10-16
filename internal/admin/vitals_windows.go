//go:build windows

package admin

import (
	"net"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformVitals uses the process times, the global memory status and the interface
// table; Windows exposes no load average to this binary, so it stays absent (I-85).
func platformVitals(m *MachineReport, tailnet *net.Interface) {
	netVitals(m, tailnet)
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

// netVitals sums the interface table's byte counters (GetIfTable2Ex, MIB_IF_ROW2).
func netVitals(m *MachineReport, tailnet *net.Interface) {
	procNet.apply(m, func() []ifCount { return ifTable(tailnet) })
}

// ifTable reads the interface table, copying its rows out before the table is freed.
func ifTable(tailnet *net.Interface) []ifCount {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfEntryNormal, &table); err != nil || table == nil {
		return nil
	}
	defer windows.NewLazySystemDLL("iphlpapi.dll").NewProc("FreeMibTable").Call(uintptr(unsafe.Pointer(table))) //nolint:errcheck,gosec // frees the table the call allocated
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)                                                     //nolint:gosec // NumEntries rows follow the header, as GetIfTable2Ex documents
	idx := uint32(0)
	if tailnet != nil && tailnet.Index > 0 {
		idx = uint32(tailnet.Index) //nolint:gosec // an interface index
	}
	return ifRows(rows, idx)
}

// ifRows reads every row but the software loopback, marking the row at tailnet (an
// interface index; 0 for none).
func ifRows(rows []windows.MibIfRow2, tailnet uint32) []ifCount {
	var ifs []ifCount
	for i := range rows {
		r := &rows[i]
		if r.Type == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		ifs = append(ifs, ifCount{ID: strconv.FormatUint(uint64(r.InterfaceIndex), 10), Rx: r.InOctets, Tx: r.OutOctets, Tailnet: tailnet != 0 && r.InterfaceIndex == tailnet})
	}
	return ifs
}
