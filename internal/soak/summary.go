package soak

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// window is a span during which a fault was in force: from a kill, partition or restart
// event to the next start or heal.
type window struct{ from, to time.Time }

func (w window) contains(t time.Time) bool {
	return !t.Before(w.from) && (w.to.IsZero() || t.Before(w.to))
}

// faultWindows derives the fault spans from the event records.
func faultWindows(records []Record) []window {
	var out []window
	open := -1
	for i := range records {
		rec := &records[i]
		if rec.Kind != "event" {
			continue
		}
		switch {
		case strings.HasPrefix(rec.Name, "kill") || rec.Name == "partition" || rec.Name == "restart all":
			if open < 0 {
				out = append(out, window{from: rec.T})
				open = len(out) - 1
			}
		case rec.Name == "start" || rec.Name == "heal":
			if open >= 0 {
				out[open].to = rec.T
				open = -1
			}
		}
	}
	return out
}

func inWindows(ws []window, t time.Time) bool {
	for _, w := range ws {
		if w.contains(t) {
			return true
		}
	}
	return false
}

// percentiles returns p50, p99 and max of ds (nearest-rank); zeros for no samples.
func percentiles(ds []time.Duration) (p50, p99, maxD time.Duration) {
	if len(ds) == 0 {
		return 0, 0, 0
	}
	s := append([]time.Duration(nil), ds...)
	slices.Sort(s)
	rank := func(p float64) time.Duration {
		i := max(int(p*float64(len(s))+0.999999)-1, 0)
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	return rank(0.5), rank(0.99), s[len(s)-1]
}

// latencyRows groups the measured operations by phase and kind, whole phase first and then
// only the operations issued while a fault was in force.
func latencyRows(lats []opLatency, phases []PhaseResult, ws []window) []LatencyRow {
	type key struct {
		phase string
		read  bool
		fault bool
	}
	groups := map[key][]time.Duration{}
	for _, l := range lats {
		groups[key{l.Phase, l.Read, false}] = append(groups[key{l.Phase, l.Read, false}], l.D)
		if inWindows(ws, l.T) {
			groups[key{l.Phase, l.Read, true}] = append(groups[key{l.Phase, l.Read, true}], l.D)
		}
	}
	var out []LatencyRow
	for _, ph := range phases {
		for _, fault := range []bool{false, true} {
			for _, read := range []bool{false, true} {
				ds := groups[key{ph.Name, read, fault}]
				if len(ds) == 0 {
					continue
				}
				kind := "write"
				if read {
					kind = "read"
				}
				p50, p99, mx := percentiles(ds)
				out = append(out, LatencyRow{Phase: ph.Name, Kind: kind, UnderFaults: fault, N: len(ds), P50: p50, P99: p99, Max: mx})
			}
		}
	}
	return out
}

// coverage lists every cited invariant and lifecycle cell with its pass and fail counts,
// invariants first, each in numeric order.
func coverage(phases []PhaseResult) []CoverageRow {
	rows := map[string]*CoverageRow{}
	for _, ph := range phases {
		for i := range ph.Checks {
			c := &ph.Checks[i]
			if c.Skip {
				continue // measured nothing: covers nothing
			}
			for id := range strings.FieldsSeq(c.Cites) {
				row, ok := rows[id]
				if !ok {
					row = &CoverageRow{ID: id}
					rows[id] = row
				}
				if c.OK == nil || *c.OK {
					row.Passed++
				} else {
					row.Failed++
				}
			}
		}
	}
	out := make([]CoverageRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return idLess(out[i].ID, out[j].ID) })
	return out
}

// idLess orders "I-n" before "L-n", numerically within a kind.
func idLess(a, b string) bool {
	ka, na := splitID(a)
	kb, nb := splitID(b)
	if ka != kb {
		return ka < kb
	}
	return na < nb
}

func splitID(id string) (kind string, n int) {
	before, after, ok := strings.Cut(id, "-")
	if !ok {
		return id, 0
	}
	for _, c := range after {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return before, n
}
