//go:build linux

package admin

import (
	"bufio"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// platformVitals reads procfs: nothing here fails loudly, a file that cannot be read
// leaves its fields absent (I-85).
func platformVitals(m *MachineReport, tailnet *net.Interface) {
	rusageVitals(m)
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		name := ""
		if tailnet != nil {
			name = tailnet.Name
		}
		if rx, tx, trx, ttx, ok := parseNetDev(string(b), name); ok {
			m.NetRx, m.NetTx, m.TailnetRx, m.TailnetTx = rx, tx, trx, ttx
		}
	}
	if f := fields("/proc/self/statm"); len(f) > 1 {
		if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			m.RSS = pages * uint64(os.Getpagesize())
		}
	}
	if f := fields("/proc/loadavg"); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			m.Load1 = v
		}
	}
	eachLine("/proc/meminfo", func(line string) {
		f := strings.Fields(line)
		if len(f) < 2 {
			return
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return
		}
		switch f[0] {
		case "MemTotal:":
			m.MemTotal = kb << 10
		case "MemAvailable:":
			m.MemAvailable = kb << 10
		}
	})
	eachLine("/proc/stat", func(line string) {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			return
		}
		var total, idle uint64
		for i, s := range f[1:] {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		m.MachineTotal, m.MachineBusy = total, total-idle
	})
	eachLine("/proc/pressure/memory", func(line string) {
		if !strings.HasPrefix(line, "some ") {
			return
		}
		for _, kv := range strings.Fields(line)[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				if p, err := strconv.ParseFloat(v, 64); err == nil {
					m.MemPressure = p
				}
			}
		}
	})
	eachLine("/proc/diskstats", func(line string) {
		f := strings.Fields(line)
		if len(f) < 10 || !wholeDisk.MatchString(f[2]) {
			return
		}
		rd, err1 := strconv.ParseUint(f[5], 10, 64) // sectors read
		wr, err2 := strconv.ParseUint(f[9], 10, 64) // sectors written
		if err1 == nil && err2 == nil {
			m.DiskRead += rd * 512
			m.DiskWritten += wr * 512
		}
	})
}

// wholeDisk matches a disk, not a partition, so bytes are not counted twice.
var wholeDisk = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

func fields(path string) []string {
	b, err := os.ReadFile(path) //nolint:gosec // a fixed /proc or /sys path
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

func eachLine(path string, fn func(line string)) {
	f, err := os.Open(path) //nolint:gosec // a fixed /proc or /sys path
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fn(sc.Text())
	}
}
