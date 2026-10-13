package webui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/stats"
)

type streamFirst struct {
	Range  map[string]any `json:"range"`
	Nodes  []any          `json:"nodes"`
	Charts []stats.Frame  `json:"charts"`
}

type streamChart struct {
	ID      string `json:"id"`
	SVG     string `json:"svg"`
	Caption string `json:"caption"`
	Empty   bool   `json:"empty"`
	Error   string `json:"error"`
}

// With stream=1 the first line (range, members, every chart's frame) is written before any
// row is read or member asked: here the members' fetch is held, and the first line is read
// all the same; then one line per chart in the first line's order, a chart without rows
// marked empty; ids= limits the charts drawn, never the first line.
// Invariant: I-258
func TestWebUI_Stats_Stream_FramesFirst(t *testing.T) {
	release := make(chan struct{})
	u, _ := statsUI(t, func(ctx context.Context, _ string, since int64) ([]stats.Row, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return []stats.Row{{T: since + 1, N: "two", M: map[string]float64{"cpu": 0.7}}}, nil
	})
	from := u.f.Clk.Now().Add(-time.Hour).Unix()
	read := func(query string, hold bool) (streamFirst, []streamChart) {
		t.Helper()
		resp := u.do(http.MethodGet, "/api/stats?from="+itoa(from)+"&to=now&stream=1"+query, nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 8<<20)
		got := make(chan bool, 1)
		go func() { got <- sc.Scan() }()
		select {
		case ok := <-got:
			if !ok {
				t.Fatalf("no first line: %v", sc.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the first line waited for the rows")
		}
		if hold {
			close(release)
		}
		var first streamFirst
		must(t, json.Unmarshal(sc.Bytes(), &first))
		var lines []streamChart
		for sc.Scan() {
			var c streamChart
			must(t, json.Unmarshal(sc.Bytes(), &c))
			lines = append(lines, c)
		}
		return first, lines
	}

	first, lines := read("", true)
	if len(first.Charts) == 0 || first.Range["tier"] == nil || len(first.Nodes) != 3 {
		t.Fatalf("first line: %+v", first)
	}
	for _, c := range first.Charts {
		if c.ID == "" || c.Title == "" || !strings.Contains(c.Frame, `class="chart pending"`) {
			t.Fatalf("a frame without its id, title or pending SVG: %+v", c)
		}
	}
	var ids []string
	for _, c := range lines {
		ids = append(ids, c.ID)
		if c.Error == "" && !c.Empty && !strings.Contains(c.SVG, `<svg class="chart"`) {
			t.Fatalf("chart %s: neither drawn, empty nor failed: %+v", c.ID, c)
		}
	}
	var want []string
	for _, c := range first.Charts {
		want = append(want, c.ID)
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("lines in order %v, want the first line's %v", ids, want)
	}
	drawn := 0
	for _, c := range lines {
		if c.SVG != "" {
			drawn++
		}
	}
	if drawn == 0 {
		t.Fatal("no chart drawn from the live rows")
	}

	// ids= limits the charts drawn, not the first line
	first, lines = read("&ids=cpu,chunks", false)
	if len(first.Charts) != len(want) {
		t.Fatalf("ids= shortened the first line to %d charts", len(first.Charts))
	}
	var got []string
	for _, c := range lines {
		got = append(got, c.ID)
	}
	if !slices.Equal(got, []string{"chunks", "cpu"}) {
		t.Fatalf("ids=cpu,chunks drew %v, want [chunks cpu] in the first line's order", got)
	}
}
