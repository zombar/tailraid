package stats

import (
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chart"
	"github.com/zombar/tailraid/internal/meta"
)

// Snapshot is what a node row is made of: the cheap parts of the status report (12 §8),
// never a chunk walk (I-191).
type Snapshot struct {
	T       time.Time
	Machine *admin.MachineReport
	Gateway *admin.GatewayReport
	Process *admin.ProcessReport
}

// NodeRow turns the current snapshot into a row. A cumulative counter becomes a rate
// against prev (bytes or chunks per second, cpu as a share of one core); the first
// snapshot after a start (prev nil) carries no rate; a vital absent from the report
// (zero in a MachineReport, which omits what the OS does not expose) is absent from the
// row; the gateway and process gauges are real values, zero included.
func NodeRow(name string, prev, cur *Snapshot) Row {
	m := map[string]float64{}
	gauge := func(k string, v float64) {
		if v != 0 {
			m[k] = v
		}
	}
	if mc := cur.Machine; mc != nil {
		gauge("load1", mc.Load1)
		rss := mc.RSS
		if rss == 0 {
			rss = mc.RSSPeak // the peak is the best macOS offers
		}
		gauge("rss", float64(rss))
		gauge("mem_avail", float64(mc.MemAvailable))
		gauge("mem_pressure", mc.MemPressure)
		gauge("disk_free", float64(mc.DiskFree))
	}
	if g := cur.Gateway; g != nil {
		m["dirty"] = float64(g.DirtyBytes)
		m["wal"] = float64(max(g.WALBytes, 0))
		m["plan"] = float64(g.PlanBytes)
		m["flush"] = float64(g.FlushBytes)
		m["prefetching"] = float64(g.Prefetching)
	}
	if p := cur.Process; p != nil {
		m["heap"] = float64(p.HeapInuse)
		m["sys"] = float64(p.Sys)
		m["goroutines"] = float64(p.Goroutines)
	}
	if prev != nil {
		dt := cur.T.Sub(prev.T).Seconds()
		if dt > 0 {
			rate := func(k string, now, before uint64) {
				if now != 0 && before != 0 && now >= before {
					m[k] = float64(now-before) / dt
				}
			}
			if pm, cm := prev.Machine, cur.Machine; pm != nil && cm != nil {
				if cm.CPUSeconds > 0 && pm.CPUSeconds > 0 && cm.CPUSeconds >= pm.CPUSeconds {
					m["cpu"] = (cm.CPUSeconds - pm.CPUSeconds) / dt
				}
				rate("disk_w", cm.DiskWritten, pm.DiskWritten)
				rate("disk_r", cm.DiskRead, pm.DiskRead)
				rate("net_rx", cm.NetRx, pm.NetRx)
				rate("net_tx", cm.NetTx, pm.NetTx)
				rate("tailnet_rx", cm.TailnetRx, pm.TailnetRx)
				rate("tailnet_tx", cm.TailnetTx, pm.TailnetTx)
			}
			if pg, cg := prev.Gateway, cur.Gateway; pg != nil && cg != nil {
				counter := func(k string, now, before uint64) {
					if now >= before {
						m[k] = float64(now-before) / dt
					}
				}
				counter("ra_fetched", cg.ReadaheadFetched, pg.ReadaheadFetched)
				counter("ra_joined", cg.ReadaheadJoined, pg.ReadaheadJoined)
				counter("bytes_r", cg.BytesRead, pg.BytesRead)
				counter("bytes_w", cg.BytesWritten, pg.BytesWritten)
			}
		}
	}
	return Row{T: cur.T.Unix(), N: name, M: m}
}

// View is the leader's cheap view of the cluster at one instant (docs/17 §1).
type View struct {
	T        time.Time
	Term     uint64
	By       string
	Health   admin.VolumeHealth // the last chunk walk (at most one a minute)
	Stats    meta.Stats
	Members  []meta.Node
	Live     func(meta.NodeIdx) int // 0 dead, 1 live, 2 suspect
	Moved    int64                  // bytes the running pass has moved; -1 while no pass runs
	PrevMove int64                  // the previous row's Moved, for the rate
	PrevT    time.Time
}

// ClusterRow builds the leader's row from its view: the volume counters, the backlog of
// the last walk, every device's liveness and every active target's slots.
func ClusterRow(v View) Row {
	c := &Cluster{
		Degraded: float64(v.Health.Degraded), Pending: float64(v.Health.RepairPending), Stale: float64(v.Health.StalePolicy), Unreadable: float64(v.Health.Unreadable),
		Stored: float64(v.Stats.StoredBytes), Logical: float64(v.Stats.LogicalBytes), Chunks: float64(v.Stats.Chunks - v.Stats.Tombstoned),
		DomainSkew: float64(v.Health.DomainSkew), DiskSkew: float64(v.Health.DiskSkew),
		Live: map[string]int{},
	}
	if v.Moved >= 0 && v.PrevMove >= 0 && v.Moved >= v.PrevMove {
		if dt := v.T.Sub(v.PrevT).Seconds(); dt > 0 {
			c.RepairMoved = float64(v.Moved-v.PrevMove) / dt
		}
	}
	hosts := map[meta.NodeIdx]string{}
	for i := range v.Members {
		n := &v.Members[i]
		if n.Kind == meta.KindDevice && n.State != meta.Removed {
			hosts[n.Idx] = n.Name
			if v.Live != nil {
				c.Live[n.Name] = v.Live(n.Idx)
			}
		}
	}
	for i := range v.Members {
		n := &v.Members[i]
		if n.Kind != meta.KindDisk || n.State == meta.Removed {
			continue
		}
		live, host := 1, hosts[n.Host]
		if v.Live != nil {
			live = v.Live(n.Host)
		}
		c.Targets = append(c.Targets, chart.Target{Name: n.Name, Host: host, Slots: v.Health.Slots[n.Idx], Capacity: n.Capacity, Live: live != 0, Active: n.State == meta.Active})
	}
	return Row{T: v.T.Unix(), Term: v.Term, By: v.By, C: c}
}

// LiveState is a device's liveness as the cluster row carries it (docs/17 §1): 0 dead,
// 1 live, 2 live but suspect (a shard RPC to it failed, docs/05 §6). Dead wins: a dead
// member keeps its suspect mark until its next good probe, and the row must say dead.
func LiveState(live, suspect bool) int {
	switch {
	case !live:
		return 0
	case suspect:
		return 2
	}
	return 1
}
