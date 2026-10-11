package stats

import (
	"sort"
	"time"
)

// Rollup folds raw rows into the tier's intervals (I-193): per interval and per writer,
// the mean of every metric over the rows that carry it, the minimum of each device's
// liveness, the last targets, the highest term; a row's t is its interval's start; an
// interval without rows yields no row. A pure function of its input, whatever the order
// the rows arrive in, so every node derives the same bytes.
func Rollup(rows []Row, t Tier) []Row {
	step := int64(t.Step() / time.Second)
	type key struct {
		start int64
		n     string // the node, or "" for the cluster rows
	}
	type acc struct {
		sum, cnt map[string]float64
		rows     []Row
	}
	groups := map[key]*acc{}
	var order []key
	for _, r := range rows {
		k := key{start: r.T - r.T%step}
		if r.C == nil {
			k.n = r.N
		}
		g, ok := groups[k]
		if !ok {
			g = &acc{sum: map[string]float64{}, cnt: map[string]float64{}}
			groups[k] = g
			order = append(order, k)
		}
		g.rows = append(g.rows, r)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].start != order[j].start {
			return order[i].start < order[j].start
		}
		return order[i].n < order[j].n
	})
	out := make([]Row, 0, len(order))
	for _, k := range order {
		g := groups[k]
		if k.n != "" {
			// sum in time order: floating-point sums depend on their order, and two nodes
			// may hold the rows in different orders
			sort.SliceStable(g.rows, func(i, j int) bool { return g.rows[i].T < g.rows[j].T })
			for _, r := range g.rows {
				for name, v := range r.M {
					g.sum[name] += v
					g.cnt[name]++
				}
			}
			m := make(map[string]float64, len(g.sum))
			for name, s := range g.sum {
				m[name] = s / g.cnt[name]
			}
			out = append(out, Row{T: k.start, N: k.n, M: m})
			continue
		}
		out = append(out, clusterRollup(k.start, g.rows))
	}
	return out
}

// clusterRollup folds the cluster rows of one interval: numeric fields by mean, Live by
// minimum, Targets from the last row by time (then by term), Term the highest.
func clusterRollup(start int64, rows []Row) Row {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].T != rows[j].T {
			return rows[i].T < rows[j].T
		}
		return rows[i].Term < rows[j].Term
	})
	var c Cluster
	live := map[string]int{}
	seen := map[string]bool{}
	var term uint64
	by := ""
	n := float64(len(rows))
	for _, r := range rows {
		x := r.C
		c.Degraded += x.Degraded / n
		c.Pending += x.Pending / n
		c.Stale += x.Stale / n
		c.Unreadable += x.Unreadable / n
		c.Stored += x.Stored / n
		c.Logical += x.Logical / n
		c.Chunks += x.Chunks / n
		c.DomainSkew += x.DomainSkew / n
		c.DiskSkew += x.DiskSkew / n
		c.RepairMoved += x.RepairMoved / n
		for name, v := range x.Live {
			if !seen[name] || v < live[name] {
				live[name], seen[name] = v, true
			}
		}
		c.Targets = x.Targets
		if r.Term >= term {
			term, by = r.Term, r.By
		}
	}
	c.Live = live
	return Row{T: start, Term: term, By: by, C: &c}
}
