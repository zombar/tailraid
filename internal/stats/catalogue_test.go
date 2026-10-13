package stats

import (
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chart"
)

var viewBoxH = regexp.MustCompile(`viewBox="0 0 760 (\d+)"`)

func heightOf(t *testing.T, svg string) int {
	t.Helper()
	m := viewBoxH.FindStringSubmatch(svg)
	if m == nil {
		t.Fatalf("no viewBox in %.80s", svg)
	}
	h, _ := strconv.Atoi(m[1])
	return h
}

// The first line of the stream lists the charts Charts draws, in its order, under the same
// ids and titles, each with a frame of its chart's height, before any row is read: the
// line charts' heights follow from their series names and the lanes' from the members;
// the disk spread's frame is an estimate from the members (its lanes are the disks the
// rows report).
// Invariant: I-258
func TestCatalogue_MatchesCharts(t *testing.T) {
	from := time.Unix(1_800_000_000, 0).UTC()
	to := from.Add(time.Hour)
	members := []string{"one", "two"}
	var rows []Row
	metrics := map[string]float64{}
	for _, m := range []string{"bytes_w", "bytes_r", "tailnet_rx", "tailnet_tx", "net_rx", "net_tx", "cpu", "load1", "rss", "mem_avail", "mem_pressure", "disk_free", "disk_w", "disk_r", "wal", "dirty", "plan", "flush", "heap", "sys", "ra_fetched", "ra_joined", "prefetching"} {
		metrics[m] = 1
	}
	for s := int64(0); s < 3600; s += 60 {
		at := from.Unix() + s
		for _, n := range members {
			rows = append(rows, Row{T: at, N: n, M: metrics})
		}
		rows = append(rows, Row{T: at, Term: 1, By: "one", C: &Cluster{
			Stored: 2, Logical: 1, Chunks: 1, Degraded: 1, Pending: 1, Stale: 1, Unreadable: 1, RepairMoved: 1,
			Live:    map[string]int{"one": 1, "two": 1},
			Targets: []chart.Target{{Name: "one/1", Host: "one", Slots: 3, Capacity: 1 << 30, Live: true, Active: true}, {Name: "two/1", Host: "two", Slots: 3, Capacity: 1 << 30, Live: true, Active: true}},
		}})
	}
	for _, node := range []string{"one", "all"} {
		drawn := Charts(rows, from, to, node)
		cat := Catalogue(node, members)
		if len(drawn) != len(cat) {
			t.Fatalf("node %s: %d charts drawn, %d in the catalogue", node, len(drawn), len(cat))
		}
		for i := range drawn {
			d, c := drawn[i], cat[i]
			if d.ID != c.ID || d.Title != c.Title {
				t.Errorf("node %s chart %d: drawn %s %q, catalogue %s %q", node, i, d.ID, d.Title, c.ID, c.Title)
				continue
			}
			// the spread's frame assumes one disk a member, which this fixture has
			if dh, ch := heightOf(t, d.SVG), heightOf(t, c.Frame); dh != ch {
				t.Errorf("node %s %s: the chart is %d high, its frame %d", node, d.ID, dh, ch)
			}
		}
	}
}
