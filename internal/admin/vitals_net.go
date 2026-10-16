package admin

import (
	"strconv"
	"strings"
	"sync"
)

// parseNetDev reads the receive and transmit bytes of every interface in a /proc/net/dev
// listing but loopback, marking the interface named tailnet ("" for none). No interface
// parsed is an empty list (I-190: absent, never zero).
func parseNetDev(text, tailnet string) []ifCount {
	var ifs []ifCount
	for line := range strings.SplitSeq(text, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "" || name == "lo" || len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		ifs = append(ifs, ifCount{ID: name, Rx: rx, Tx: tx, Tailnet: name == tailnet})
	}
	return ifs
}

// ifCount is one interface's cumulative byte counters as the OS reports them.
type ifCount struct {
	ID      string // the OS's name or index for the interface
	Rx, Tx  uint64
	Tailnet bool // the interface that holds the node's tailnet address
}

// netCounters turns the OS's per-interface counters into the report's four, which never
// decrease within one process (I-85, I-376). The first report carries the OS's sums; each
// later one adds every interface's increase since the report before: a counter that
// dropped across 2^32 wrapped (macOS keeps a utun's counters in 32 bits, though the list
// carries 64) and adds what crossed the wrap, one that dropped otherwise was re-created
// and adds its new value, and an interface that is gone keeps what it added.
type netCounters struct {
	mu               sync.Mutex
	last             map[string][2]uint64 // interface → its rx and tx in the last report
	rx, tx, trx, ttx uint64
	tailnet          bool // a tailnet interface has been seen
}

// procNet is the process's own: every report that carries machine vitals reads it.
var procNet netCounters

// apply sets m's network counters from what read returns; with no interface it leaves
// them absent. read runs under the lock: two reports read at once (the status refresher
// and the stats sampler) must apply in the order they read, or the earlier reading would
// look like a counter that started again.
func (c *netCounters) apply(m *MachineReport, read func() []ifCount) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ifs := read()
	if len(ifs) == 0 {
		return
	}
	last := make(map[string][2]uint64, len(ifs))
	for _, i := range ifs {
		drx, dtx := i.Rx, i.Tx // an interface not in the last report counts from zero
		if prev, ok := c.last[i.ID]; ok {
			drx, dtx = counterStep(prev[0], i.Rx), counterStep(prev[1], i.Tx)
		}
		last[i.ID] = [2]uint64{i.Rx, i.Tx}
		c.rx, c.tx = c.rx+drx, c.tx+dtx
		if i.Tailnet {
			c.trx, c.ttx, c.tailnet = c.trx+drx, c.ttx+dtx, true
		}
	}
	c.last = last
	m.NetRx, m.NetTx = c.rx, c.tx
	if c.tailnet {
		m.TailnetRx, m.TailnetTx = c.trx, c.ttx
	}
}

// counterStep is how much a counter that read prev and now reads cur moved: a 32-bit
// counter that wrapped (prev below 2^32 and the wrap less than 2^31 away) moved across
// the wrap; any other drop is a counter that started again from zero.
func counterStep(prev, cur uint64) uint64 {
	const wrap = 1 << 32
	switch {
	case cur >= prev:
		return cur - prev
	case prev < wrap && wrap-prev+cur < wrap/2:
		return wrap - prev + cur
	default:
		return cur
	}
}
