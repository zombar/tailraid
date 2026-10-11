package webui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/stats"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// statsUI is a UI whose stats query reads the fabric's volume and knows three members.
func statsUI(t *testing.T, fetch stats.Fetcher) (*ui, stats.VFS) {
	t.Helper()
	var f *testharness.Fabric
	var vol stats.VFS
	u := newUIWith(t, func(fb *testharness.Fabric) { f = fb }, func(o *webui.Options) {
		vol = stats.VFS{FS: o.FS, St: o.Store, Rd: f.ReaderNoCache()}
		o.Stats = &stats.Query{
			Vol: vol, Clk: o.Clk, Keep: stats.DefaultKeep, CacheBytes: 32 << 20, Fetch: fetch,
			Members: func() []stats.Member {
				return []stats.Member{{Name: "one", Addr: "100.64.0.1:20481", Live: true, Self: true, Web: 20482}, {Name: "two", Addr: "100.64.0.2:20481", Live: true, Web: 20482}, {Name: "three", Addr: "100.64.0.3:20481"}}
			},
			Self: func(since int64) []stats.Row {
				return []stats.Row{{T: since + 1, N: "one", M: map[string]float64{"cpu": 0.5}}}
			},
		}
	})
	u.fs.SetFlusher(u.f.Flusher())
	return u, vol
}

// /api/stats validates its range (the boundary table), answers 503 without a query, and
// draws the range's charts from the archive with the live edge when to is now; the
// member list carries each member's address, liveness and web port for the page's links.
// Invariant: I-196, I-198
func TestWebUI_Stats_Route(t *testing.T) {
	fetched := 0
	u, vol := statsUI(t, func(_ context.Context, addr string, since int64) ([]stats.Row, error) {
		fetched++
		return []stats.Row{{T: since + 1, N: "two", M: map[string]float64{"cpu": 0.7}}}, nil
	})
	now := u.f.Clk.Now().UTC()
	hour := now.Truncate(time.Hour).Add(-2 * time.Hour)
	var rows []stats.Row
	for s := 0; s < 3600; s += 10 {
		at := hour.Add(time.Duration(s) * time.Second)
		rows = append(rows,
			stats.Row{T: at.Unix(), N: "one", M: map[string]float64{"cpu": 0.1, "bytes_w": 1 << 20}},
			stats.Row{T: at.Unix(), Term: 2, By: "one", C: &stats.Cluster{Chunks: 10, Stored: 1e6, Logical: 8e5, Live: map[string]int{"one": 1, "two": 1}}})
	}
	var buf bytes.Buffer
	must(t, stats.Encode(&buf, rows))
	must(t, vol.WriteFile(context.Background(), path.Join(stats.Root, "10s", "one", stats.SegmentName(stats.Raw, hour, "one")), buf.Bytes()))
	from := hour.Unix()
	for _, tc := range []struct {
		name, query string
		code        int
	}{
		{"no from", "", 400},
		{"junk from", "from=soon", 400},
		{"from equals to", "from=100&to=100", 400},
		{"from after to", "from=200&to=100", 400},
		{"range past the history", "from=1&to=" + itoa(now.Unix()), 400},
		{"a day", "from=" + itoa(from) + "&to=" + itoa(from+86400), 200},
		{"to now", "from=" + itoa(from) + "&to=now", 200},
		{"no to", "from=" + itoa(from), 200},
	} {
		code, _, body := u.get("/api/stats?" + tc.query)
		if code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, code, body, tc.code)
		}
	}
	if fetched != 1 { // the two live rows of the table share one fan-out (I-196)
		t.Fatalf("%d fetches for two live requests within %v", fetched, stats.FetchShare)
	}
	code, _, body := u.get("/api/stats?from=" + itoa(from) + "&to=" + itoa(hour.Add(time.Hour).Unix()))
	if code != 200 {
		t.Fatalf("archive: %d %s", code, body)
	}
	var out struct {
		Range struct {
			From, To, Step int64
			Tier           string
			Live           bool
		}
		Node  string
		Nodes []struct {
			Name, Addr string
			Live, Self bool
			Web        int
		}
		Charts []stats.Chart
	}
	must(t, json.Unmarshal(body, &out))
	if out.Range.Tier != "10s" || out.Range.Step != 10 || out.Range.Live || out.Node != "one" || len(out.Nodes) != 3 || out.Nodes[1].Addr != "100.64.0.2" || out.Nodes[1].Web != 20482 || out.Nodes[2].Live || !out.Nodes[0].Self {
		t.Fatalf("envelope %+v", out)
	}
	ids := map[string]bool{}
	for _, c := range out.Charts {
		ids[c.ID] = true
		if !strings.HasPrefix(c.SVG, `<svg class="chart"`) || c.Title == "" {
			t.Fatalf("chart %+v", c)
		}
	}
	if !ids["stored"] || !ids["liveness"] || !ids["throughput"] || !ids["cpu"] {
		t.Fatalf("charts %v", ids)
	}
	if fetched != 1 {
		t.Fatal("an archive-only range asked the members")
	}
	// to now, past the share window: the live edge of the live members, self included,
	// the dead one skipped
	u.f.Clk.Advance(stats.FetchShare)
	code, _, body = u.get("/api/stats?from=" + itoa(now.Add(-time.Hour).Unix()) + "&to=now&node=two")
	must(t, json.Unmarshal(body, &out))
	if code != 200 || !out.Range.Live || out.Node != "two" || fetched != 2 {
		t.Fatalf("live: %d live=%v node=%s fetched=%d", code, out.Range.Live, out.Node, fetched)
	}
	// without a query: 503
	plain := newUI(t)
	if code, _, _ := plain.get("/api/stats?from=1&to=2"); code != 503 {
		t.Fatalf("no query: %d", code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
