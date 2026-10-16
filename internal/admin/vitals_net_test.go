package admin

import (
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

const netDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1234567    9000    0    0    0     0          0         0  1234567    9000    0    0    0     0       0          0
  eth0: 500000000 400000    0    0    0     0          0      1000 120000000 300000    0    0    0     0       0          0
tailscale0:  7000000   50000    0    0    0     0          0         0   3000000   40000    0    0    0     0       0          0
  junk: x y
  short: 1 2 3
`

// The /proc/net/dev parser sums every interface but loopback, picks out the tailnet
// interface by name, ignores malformed lines, and reports nothing for an empty listing.
// Invariant: I-190
func TestVitals_NetworkParsers_PerOS(t *testing.T) {
	for _, tc := range []struct {
		name, text, tailnet string
		rx, tx, trx, ttx    uint64
		ok                  bool
	}{
		{"fixture, tailscale0", netDevFixture, "tailscale0", 507000000, 123000000, 7000000, 3000000, true},
		{"fixture, no tailnet", netDevFixture, "", 507000000, 123000000, 0, 0, true},
		{"fixture, unknown tailnet", netDevFixture, "wg0", 507000000, 123000000, 0, 0, true},
		{"loopback only", "lo: 5 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0\n", "lo", 0, 0, 0, 0, false},
		{"empty", "", "", 0, 0, 0, 0, false},
		{"header only", netDevFixture[:120], "", 0, 0, 0, 0, false},
	} {
		rx, tx, trx, ttx, ok := sumIfs(parseNetDev(tc.text, tc.tailnet))
		if rx != tc.rx || tx != tc.tx || trx != tc.trx || ttx != tc.ttx || ok != tc.ok {
			t.Errorf("%s: got %d %d %d %d %v, want %d %d %d %d %v", tc.name, rx, tx, trx, ttx, ok, tc.rx, tc.tx, tc.trx, tc.ttx, tc.ok)
		}
	}
}

// The tailnet interface is found by the address it holds, the same way on every OS; the
// zero address and an address nobody holds give none.
// Invariant: I-190
func TestVitals_TailnetInterface_Match(t *testing.T) {
	if tailnetInterface(netip.Addr{}) != nil {
		t.Error("the zero address matched an interface")
	}
	if tailnetInterface(netip.MustParseAddr("192.0.2.1")) != nil {
		t.Error("a documentation address matched an interface")
	}
	lo := tailnetInterface(netip.MustParseAddr("127.0.0.1"))
	if lo == nil {
		t.Skip("no loopback address on this host")
	}
	if lo.Flags&net.FlagLoopback == 0 {
		t.Errorf("%s holds 127.0.0.1 but is not a loopback interface", lo.Name)
	}
}

// sumIfs is a fresh process's first report of ifs: the OS's sums (ok false for none).
func sumIfs(ifs []ifCount) (rx, tx, trx, ttx uint64, ok bool) {
	var c netCounters
	var m MachineReport
	c.apply(&m, func() []ifCount { return ifs })
	return m.NetRx, m.NetTx, m.TailnetRx, m.TailnetTx, len(ifs) > 0
}

// The report's network counters never decrease within one process: a counter that wraps
// at 2^32 adds what crossed the wrap (a Mac's tailnet tx went from 4.06 GB to 34 MB in
// 30 s, a utun's 32-bit counter), one that drops otherwise was re-created and adds its new
// value, and an interface that vanishes keeps what it added.
// Invariant: I-376
func TestVitals_NetCounters_WrapAndReset(t *testing.T) {
	const wrap = 1 << 32
	type report struct{ rx, tx, trx, ttx uint64 }
	for _, tc := range []struct {
		name  string
		steps [][]ifCount
		want  []report
	}{
		{
			"first report is the OS's sums, then increases add",
			[][]ifCount{
				{{ID: "en0", Rx: 1000, Tx: 200}, {ID: "utun4", Rx: 50, Tx: 7, Tailnet: true}},
				{{ID: "en0", Rx: 1500, Tx: 300}, {ID: "utun4", Rx: 60, Tx: 9, Tailnet: true}},
			},
			[]report{{1050, 207, 50, 7}, {1560, 309, 60, 9}},
		},
		{
			"a 32-bit counter wraps",
			[][]ifCount{
				{{ID: "utun4", Rx: 10, Tx: 4_061_426_688, Tailnet: true}},
				{{ID: "utun4", Rx: 20, Tx: 33_842_176, Tailnet: true}},
			},
			[]report{{10, 4_061_426_688, 10, 4_061_426_688}, {20, 4_061_426_688 + (wrap - 4_061_426_688) + 33_842_176, 20, 4_061_426_688 + (wrap - 4_061_426_688) + 33_842_176}},
		},
		{
			"a counter at wrap-1 steps to 0",
			[][]ifCount{{{ID: "a", Rx: wrap - 1}}, {{ID: "a", Rx: 0}}},
			[]report{{wrap - 1, 0, 0, 0}, {wrap, 0, 0, 0}},
		},
		{
			"a 64-bit counter that drops was re-created",
			[][]ifCount{{{ID: "eth0", Rx: 5 << 40, Tx: 1 << 40}}, {{ID: "eth0", Rx: 100, Tx: 10}}},
			[]report{{5 << 40, 1 << 40, 0, 0}, {5<<40 + 100, 1<<40 + 10, 0, 0}},
		},
		{
			"a small counter far below its last value restarted, not wrapped",
			[][]ifCount{{{ID: "a", Rx: 1000}}, {{ID: "a", Rx: 10}}},
			[]report{{1000, 0, 0, 0}, {1010, 0, 0, 0}},
		},
		{
			"an interface that vanishes keeps its bytes, a new one counts from zero",
			[][]ifCount{
				{{ID: "en0", Rx: 1000}, {ID: "en5", Rx: 400}},
				{{ID: "en0", Rx: 1100}},
				{{ID: "en0", Rx: 1200}, {ID: "en7", Rx: 30}},
			},
			[]report{{1400, 0, 0, 0}, {1500, 0, 0, 0}, {1630, 0, 0, 0}},
		},
		{
			"the tailnet interface re-created under a new index",
			[][]ifCount{
				{{ID: "4", Rx: 900, Tailnet: true}},
				{{ID: "9", Rx: 15, Tailnet: true}},
			},
			[]report{{900, 0, 900, 0}, {915, 0, 915, 0}},
		},
		{
			"no interface leaves the counters absent",
			[][]ifCount{nil},
			[]report{{0, 0, 0, 0}},
		},
	} {
		var c netCounters
		for i, ifs := range tc.steps {
			var m MachineReport
			c.apply(&m, func() []ifCount { return ifs })
			if got := (report{m.NetRx, m.NetTx, m.TailnetRx, m.TailnetTx}); got != tc.want[i] {
				t.Errorf("%s, report %d: got %+v, want %+v", tc.name, i, got, tc.want[i])
			}
		}
	}
}

// Reports built at once (the status refresher and the stats sampler) share the process's
// counters: each reads the OS under the counters' lock, so a reading never applies after
// a later one (it would look like a restart and count its whole value again).
// Invariant: I-376
func TestVitals_NetCounters_Concurrent(t *testing.T) {
	var c netCounters
	var reading atomic.Uint64
	reading.Store(1000)
	read := func() []ifCount {
		r := reading.Add(10)
		runtime.Gosched() // a reader preempted between its read and its apply
		return []ifCount{{ID: "en0", Rx: r}}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			var prev uint64
			for range 200 {
				var m MachineReport
				c.apply(&m, read)
				if m.NetRx < prev {
					t.Errorf("rx went back from %d to %d", prev, m.NetRx)
					return
				}
				prev = m.NetRx
			}
		})
	}
	wg.Wait()
	var m MachineReport
	c.apply(&m, read)
	if want := reading.Load(); m.NetRx != want {
		t.Fatalf("rx %d, want %d (every increase counted once)", m.NetRx, want)
	}
}
