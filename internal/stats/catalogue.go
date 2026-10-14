package stats

import "github.com/zombar/tailraid/internal/chart"

// Frame is one chart of the page before its rows are read (docs/17 §3, I-258): its id and
// title, and the empty SVG the page draws until the chart arrives.
type Frame struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Frame string `json:"frame"`
}

// Catalogue is every chart Charts can draw, in its order and under its ids and titles,
// each with a frame of the chart's height, known before any row is read: a line chart's
// height follows from its series names (the metrics, or the members for node "all"), the
// liveness lanes' from the members; the disk spread's is estimated as one disk a member,
// since its lanes are the disks the rows report. A chart the rows turn out to have nothing
// for is in the catalogue all the same; the stream says so on its line.
func Catalogue(node string, members []string) []Frame {
	var out []Frame
	line := func(id, title string, names ...string) {
		out = append(out, Frame{ID: id, Title: title, Frame: chart.Frame(title, chart.LineHeight(names))})
	}
	line("stored", "Stored vs logical bytes (dedup)", "stored bytes", "logical bytes")
	line("chunks", "Chunks", "live chunks")
	line("backlog", "Repair backlog", "degraded", "repair pending", "stale policy", "unreadable")
	line("moved", "Repair bytes moved", "bytes moved MiB/s")
	out = append(out,
		Frame{ID: "liveness", Title: "Node liveness as seen by the leader", Frame: chart.Frame("Node liveness as seen by the leader", chart.LanesHeight(len(members)))},
		Frame{ID: "spread", Title: "Data spread across disks", Frame: chart.Frame("Data spread across disks", chart.SpreadHeight(members))})
	line("slots", "Shard slots per node", members...)
	ns := func(id, title string, labels ...string) {
		if node == "all" {
			line(id, title, members...)
			return
		}
		line(id, title, labels...)
	}
	ns("throughput", "Client throughput", "write MiB/s", "read MiB/s")
	ns("tailnet", "Network (mesh)", "mesh rx MiB/s", "mesh tx MiB/s")
	ns("net", "Network (all interfaces)", "rx MiB/s", "tx MiB/s")
	ns("cpu", "CPU", "process % of one CPU", "load (1 min)")
	ns("memory", "Memory", "resident set", "memory available")
	ns("pressure", "Memory pressure", "memory pressure (PSI some, avg10 %)")
	ns("diskfree", "Data disk free", "data disk free")
	ns("diskio", "Disk throughput", "disk write MiB/s", "disk read MiB/s")
	ns("wal", "Overlay WAL depth", "overlay WAL bytes", "dirty bytes", "plan bytes", "flush buffers")
	ns("process", "Process memory", "heap in use", "sys")
	ns("readahead", "Readahead", "fetched chunks/s", "joined READs/s", "prefetching")
	return out
}
