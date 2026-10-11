package stats

import (
	"math"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
)

// A node row carries each cumulative counter as a rate against the previous snapshot,
// the first snapshot carries no rate, a vital the OS does not expose is absent (never
// zero), and the gateway and process gauges are real values, zero included.
// Invariant: I-191
func TestSampler_NodeRow_RatesAndAbsence(t *testing.T) {
	t0 := time.Unix(1760184000, 0)
	mac := &admin.MachineReport{CPUSeconds: 100, Load1: 0.8, RSSPeak: 400e6, MemTotal: 8e9, MemAvailable: 2e9, DiskFree: 5e11, DiskTotal: 1e12, NetRx: 1000, NetTx: 500}
	gw := &admin.GatewayReport{DirtyBytes: 0, WALBytes: 0, ReadaheadFetched: 10, ReadaheadJoined: 8, BytesRead: 1 << 20, BytesWritten: 0, Prefetching: 0}
	first := &Snapshot{T: t0, Machine: mac, Gateway: gw, Process: &admin.ProcessReport{HeapInuse: 90e6, Sys: 180e6, Goroutines: 200}}
	r1 := NodeRow("jockey", nil, first)
	if r1.T != t0.Unix() || r1.N != "jockey" || r1.C != nil {
		t.Fatalf("row %+v", r1)
	}
	for _, rate := range []string{"cpu", "disk_w", "disk_r", "net_rx", "net_tx", "tailnet_rx", "ra_fetched", "bytes_r", "bytes_w"} {
		if _, ok := r1.M[rate]; ok {
			t.Errorf("first row carries the rate %s", rate)
		}
	}
	for _, absent := range []string{"rss_peak", "mem_pressure", "disk_w", "machine"} {
		if _, ok := r1.M[absent]; ok {
			t.Errorf("absent vital %s in the row", absent)
		}
	}
	if r1.M["rss"] != 400e6 || r1.M["load1"] != 0.8 || r1.M["mem_avail"] != 2e9 || r1.M["disk_free"] != 5e11 {
		t.Errorf("gauges %+v", r1.M)
	}
	if v, ok := r1.M["dirty"]; !ok || v != 0 {
		t.Errorf("the gateway's zero is a real value: %+v", r1.M)
	}
	if r1.M["heap"] != 90e6 || r1.M["goroutines"] != 200 || r1.M["prefetching"] != 0 {
		t.Errorf("process %+v", r1.M)
	}
	// ten seconds on: rates from the differences; a counter that went backwards (a
	// restart) carries no rate; a counter the OS stopped exposing carries none
	mac2 := &admin.MachineReport{CPUSeconds: 103, Load1: 0.9, RSSPeak: 400e6, MemTotal: 8e9, MemAvailable: 2e9, DiskFree: 5e11, DiskTotal: 1e12, NetRx: 1000 + 20480, NetTx: 400, TailnetRx: 50}
	gw2 := &admin.GatewayReport{ReadaheadFetched: 30, ReadaheadJoined: 8, BytesRead: 1<<20 + 10240, BytesWritten: 5120, Prefetching: 2}
	second := &Snapshot{T: t0.Add(10 * time.Second), Machine: mac2, Gateway: gw2, Process: first.Process}
	r2 := NodeRow("jockey", first, second)
	for k, want := range map[string]float64{"cpu": 0.3, "net_rx": 2048, "ra_fetched": 2, "ra_joined": 0, "bytes_r": 1024, "bytes_w": 512, "prefetching": 2} {
		if got, ok := r2.M[k]; !ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v (%v), want %v", k, got, ok, want)
		}
	}
	for _, absent := range []string{"net_tx", "tailnet_rx", "disk_w", "disk_r"} {
		if _, ok := r2.M[absent]; ok {
			t.Errorf("%s has a rate it should not: %v", absent, r2.M[absent])
		}
	}
	// a zero interval carries no rate
	if r := NodeRow("jockey", second, second); len(r.M) == 0 || r.M["cpu"] != 0 {
		if _, ok := r.M["cpu"]; ok {
			t.Errorf("a zero interval produced a rate: %+v", r.M)
		}
	}
	// nothing measured: a row with no metrics, never a crash
	if r := NodeRow("jockey", nil, &Snapshot{T: t0}); len(r.M) != 0 {
		t.Errorf("empty snapshot: %+v", r.M)
	}
}

// The cluster row carries the volume counters, the last walk's backlog, every device's
// liveness and every disk's slots with its host, the running pass's rate, the term.
// Invariant: I-191
func TestSampler_ClusterRow(t *testing.T) {
	t0 := time.Unix(1760184000, 0)
	members := []meta.Node{
		{Idx: 1, Name: "jockey", Kind: meta.KindDevice, State: meta.Active},
		{Idx: 2, Name: "honker", Kind: meta.KindDevice, State: meta.Active},
		{Idx: 3, Name: "gone", Kind: meta.KindDevice, State: meta.Removed},
		{Idx: 4, Name: "jockey/1", Kind: meta.KindDisk, Host: 1, State: meta.Active, Capacity: 5e11},
		{Idx: 5, Name: "honker/1", Kind: meta.KindDisk, Host: 2, State: meta.Draining, Capacity: 1e12},
		{Idx: 6, Name: "gone/1", Kind: meta.KindDisk, Host: 3, State: meta.Removed, Capacity: 1},
	}
	live := func(i meta.NodeIdx) int {
		switch i {
		case 1:
			return 1
		case 2:
			return 2
		}
		return 0
	}
	v := View{
		T: t0, Term: 12, By: "jockey",
		Health:  admin.VolumeHealth{Degraded: 37, RepairPending: 30, Unreadable: 1, StalePolicy: 2, DomainSkew: 3, DiskSkew: 4, Slots: map[meta.NodeIdx]int{4: 15020, 5: 9000}},
		Stats:   meta.Stats{Chunks: 48423, Tombstoned: 212, LogicalBytes: 44e9, StoredBytes: 60e9},
		Members: members, Live: live, Moved: 10 << 20, PrevMove: 5 << 20, PrevT: t0.Add(-10 * time.Second),
	}
	r := ClusterRow(v)
	c := r.C
	if r.T != t0.Unix() || r.Term != 12 || r.By != "jockey" || c == nil {
		t.Fatalf("row %+v", r)
	}
	if c.Degraded != 37 || c.Pending != 30 || c.Unreadable != 1 || c.Stale != 2 || c.DomainSkew != 3 || c.DiskSkew != 4 || c.Chunks != 48211 || c.Stored != 60e9 || c.Logical != 44e9 {
		t.Errorf("counters %+v", c)
	}
	if c.RepairMoved != float64(5<<20)/10 {
		t.Errorf("repair_moved %v", c.RepairMoved)
	}
	if len(c.Live) != 2 || c.Live["jockey"] != 1 || c.Live["honker"] != 2 {
		t.Errorf("live %v", c.Live)
	}
	if len(c.Targets) != 2 || c.Targets[0].Name != "jockey/1" || c.Targets[0].Host != "jockey" || c.Targets[0].Slots != 15020 || !c.Targets[0].Active || !c.Targets[0].Live ||
		c.Targets[1].Name != "honker/1" || c.Targets[1].Active || c.Targets[1].Capacity != 1e12 {
		t.Errorf("targets %+v", c.Targets)
	}
	// no pass running: no rate
	v.Moved = -1
	if ClusterRow(v).C.RepairMoved != 0 {
		t.Error("a rate without a pass")
	}
}
