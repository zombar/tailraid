package soak

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The lookdev of the report's mesh section (docs/assets/lookdev/soak-mesh.html, docs/09
// §6) carries this package's rendering of one fixed run, so the signed-off markup and the
// code agree; UPDATE_LOOKDEV=1 rewrites the page's chart blocks and tables from the data.
const meshLookdev = "../../docs/assets/lookdev/soak-mesh.html"

// meshLookdevSummary is the fixed run: five devices over twelve minutes, every pair lan,
// n1↔n2 cut by phase 26 at 6:00 and back at 8:30, n2 restarted at 9:30, the late device
// n6 joining in phase 25 at 2:00 and removed at 4:30.
func meshLookdevSummary() Summary {
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := Summary{Started: start, Duration: 12 * time.Minute}
	names := []string{"n1", "n2", "n3", "n4", "n5"}
	at := func(sec float64) time.Time { return start.Add(time.Duration(sec * float64(time.Second))) }
	for sec := 0.0; sec <= 720; sec += 5 {
		paths := map[string]string{}
		bytes := map[string][2]uint64{}
		all := names
		if sec >= 120 && sec < 270 {
			all = append(append([]string(nil), names...), "n6")
		}
		for _, a := range all {
			for _, b := range all {
				if a == b {
					continue
				}
				class := "lan"
				if (a == "n1" && b == "n2" || a == "n2" && b == "n1") && sec >= 360+75 && sec < 510 {
					class = "none"
				}
				if a == "n6" || b == "n6" {
					class = "wan"
				}
				if b == "n2" && sec >= 570 && sec < 580 {
					class = "none"
				}
				paths[a+">"+b] = class
			}
			rate := uint64(1 << 20)
			if a == "n3" {
				rate = 6 << 20
			}
			bytes[a] = [2]uint64{uint64(sec) * rate, uint64(sec) * rate / 3}
		}
		s.Records = append(s.Records, Record{T: at(sec), Kind: "sample", S: &Sample{Paths: paths, MeshBytes: bytes}})
	}
	s.Records = append(s.Records,
		Record{T: at(150), Kind: "event", Name: meshJoinEvent, Text: "n6", Attrs: map[string]string{"late": "n6", "pending_ms": "412", "pending_on": "n2 n3 n5", "join_ms": "3180", "handshake_ms": "4930", "log_ms": "7210"}},
		Record{T: at(300), Kind: "event", Name: meshPathsEvent, Text: "every pair", Attrs: map[string]string{
			"n1>n2": "lan 172.30.0.12:20483 12s", "n1>n3": "lan 172.30.0.13:20483 4s", "n2>n1": "lan 172.30.0.11:20483 12s", "n2>n3": "lan 172.30.0.13:20483 21s", "n3>n1": "lan 172.30.0.11:20483 4s", "n3>n2": "lan 172.30.0.12:20483 21s",
		}})
	return s
}

var meshChartBlock = regexp.MustCompile(`(?s)<svg class="chart".*?</svg>`)

func TestReport_MeshLookdev(t *testing.T) {
	page, err := os.ReadFile(meshLookdev)
	if err != nil {
		t.Fatal(err)
	}
	charts, pairs, join := meshFigures(meshLookdevSummary())
	if len(charts) != 2 || len(pairs) != 6 || join == nil || join.Pending != "412 ms" || join.Join != "3.2 s" {
		t.Fatalf("figures: %d charts, %d pairs, join %+v", len(charts), len(pairs), join)
	}
	blocks := meshChartBlock.FindAllStringIndex(string(page), -1)
	if len(blocks) != len(charts) {
		t.Fatalf("the page has %d chart blocks, the data %d", len(blocks), len(charts))
	}
	if os.Getenv("UPDATE_LOOKDEV") != "" {
		var b strings.Builder
		last := 0
		for i, bl := range blocks {
			b.WriteString(string(page[last:bl[0]]))
			b.WriteString(string(charts[i]))
			last = bl[1]
		}
		b.WriteString(string(page[last:]))
		if err := os.WriteFile(meshLookdev, []byte(b.String()), 0o644); err != nil { //nolint:gosec // the lookdev page
			t.Fatal(err)
		}
		t.Log("rewrote the lookdev's chart blocks")
	}
	for i, bl := range blocks {
		if got := string(page[bl[0]:bl[1]]); got != string(charts[i]) {
			t.Errorf("chart %d differs from the package's rendering; run with UPDATE_LOOKDEV=1 to rewrite the page", i)
		}
	}
	for _, want := range []string{"<td>n1</td><td>n2</td><td>lan</td>", "<td class=\"num\">412 ms</td>", "n2 n3 n5"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// Without a mesh in the records there is no section.
func TestReport_MeshFiguresOffAMesh(t *testing.T) {
	charts, pairs, join := meshFigures(Summary{Records: []Record{{Kind: "sample", S: &Sample{}}}})
	if charts != nil || pairs != nil || join != nil {
		t.Fatal("figures drawn off a mesh")
	}
}
