package admin

import (
	"net"
	"net/netip"

	"github.com/zombar/tailraid/internal/sysfs"
)

// MachineReport is what the operating system exposes about the node to a pure-Go binary
// (docs/12 §8, I-85). Every field is a measurement or absent: a vital the OS does not
// expose is omitted, never zero. The cumulative counters (CPU seconds, machine ticks,
// disk bytes) are turned into rates by whoever samples two reports.
type MachineReport struct {
	CPUSeconds   float64 `json:"cpu_seconds,omitempty"`         // process user+system time, cumulative
	MachineBusy  uint64  `json:"machine_busy_ticks,omitempty"`  // Linux: every CPU's non-idle ticks, cumulative
	MachineTotal uint64  `json:"machine_total_ticks,omitempty"` // Linux: every CPU's ticks, cumulative
	Load1        float64 `json:"load1,omitempty"`               // 1-minute load average
	RSS          uint64  `json:"rss,omitempty"`                 // resident set now (Linux)
	RSSPeak      uint64  `json:"rss_peak,omitempty"`            // resident set peak (unix)
	MemTotal     uint64  `json:"mem_total,omitempty"`
	MemAvailable uint64  `json:"mem_available,omitempty"`
	MemPressure  float64 `json:"mem_pressure,omitempty"` // Linux PSI: share of the last 10 s some task stalled on memory, percent
	DiskFree     uint64  `json:"disk_free,omitempty"`    // the data disk, bytes available to this process
	DiskTotal    uint64  `json:"disk_total,omitempty"`
	DiskWritten  uint64  `json:"disk_written_bytes,omitempty"` // Linux: every disk, cumulative
	DiskRead     uint64  `json:"disk_read_bytes,omitempty"`
	NetRx        uint64  `json:"net_rx_bytes,omitempty"` // every interface but loopback, cumulative (I-190)
	NetTx        uint64  `json:"net_tx_bytes,omitempty"`
	MeshRx       uint64  `json:"mesh_rx_bytes,omitempty"` // the interface that holds the tailnet address
	MeshTx       uint64  `json:"mesh_tx_bytes,omitempty"`
	// Paths are the device's data directory, disks and mirrors with the full-disk
	// encryption under each (docs/08 §Files on disk, I-341); filled by the node, hourly.
	Paths []PathReport `json:"paths,omitempty"`
}

// Vitals measures the node: dataDir names the disk, meshIP the address whose interface
// the tailnet counters follow (the zero Addr leaves them absent). It never fails; what
// cannot be measured stays absent.
func Vitals(dataDir string, meshIP netip.Addr) *MachineReport {
	m := &MachineReport{}
	if free, total, err := (sysfs.OS{}).Free(dataDir); err == nil {
		m.DiskFree, m.DiskTotal = free, total
	}
	platformVitals(m, meshInterface(meshIP))
	return m
}

// meshInterface is the interface that holds ip, found through the standard library so
// the match is the same on every OS; nil when ip is unset or no interface holds it.
func meshInterface(ip netip.Addr) *net.Interface {
	if !ip.IsValid() {
		return nil
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range ifs {
		addrs, err := ifs[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if pfx, err := netip.ParsePrefix(a.String()); err == nil && pfx.Addr().Unmap() == ip.Unmap() {
				return &ifs[i]
			}
		}
	}
	return nil
}
