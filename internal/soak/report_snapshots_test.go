package soak

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Phase 19's figures: the mirror's used bytes ride the samples through JSON, and the
// report draws the bytes chart with a marker per labelled snapshot and the offline band,
// the bars of what each snapshot added (the first one grey), and the round-trip table
// with its closing line; a run that recorded no snapshot draws none of it, and the
// snapshot events never become throughput markers.
//
// Invariant: I-180
func TestReport_SnapshotFigures(t *testing.T) {
	s := Sample{Snapshots: 2, MirrorUsed: map[string]int64{"n2/m1": 3 << 20}}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Sample
	if err := json.Unmarshal(b, &back); err != nil || back.Snapshots != 2 || back.MirrorUsed["n2/m1"] != 3<<20 {
		t.Fatalf("sample round trip: %+v %v (%s)", back, err, b)
	}
	started := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	snap := func(sec int, name, label, newBytes, read string) Record {
		return Record{Kind: "event", Name: snapshotEvent, T: started.Add(100 * time.Second), Text: name, Attrs: map[string]string{
			"mirror": "n2-m1", "label": label, "at": strconv.FormatInt(started.Add(time.Duration(sec)*time.Second).UnixNano(), 10),
			"index": "1234", "files": "12", "bytes": "1048576", "new_bytes": newBytes, "state": "complete", "auto": strconv.FormatBool(label == "automatic"), "read": read,
		}}
	}
	sum := Summary{Started: started, Duration: 120 * time.Second, Records: []Record{
		{Kind: "sample", T: started.Add(10 * time.Second), S: &Sample{MirrorUsed: map[string]int64{"n2/m1": 0}}},
		{Kind: "sample", T: started.Add(40 * time.Second), S: &s},
		{Kind: "event", Name: mirrorOffline, T: started.Add(60 * time.Second), Text: "n2/m1"},
		{Kind: "event", Name: mirrorOnline, T: started.Add(80 * time.Second), Text: "n2/m1"},
		snap(20, "soak-1", "first", "1048576", "exact · <b>from the mirror</b>"),
		snap(30, "2026-10-12-0900Z", "automatic", "0", ""),
		snap(50, "soak-2", "after 5 changes", "65536", "exact"),
		snap(90, "2026-10-12-0901Z", "catch-up on attach", "4096", ""),
		{Kind: "event", Name: roundTripEvent, T: started.Add(100 * time.Second), Text: "GC collected 3 of the 3 overwritten chunks"},
	}}
	var out bytes.Buffer
	if err := Render(&out, sum); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		">n2/m1 used<", "bytes on the disk (mirrors[].used)", ">first sync<", ">after 5 changes<", ">catch-up on attach<", ">offline<",
		"What each snapshot added to its mirror", `class="chart-off"><title>soak-1 (first): 1.0 MiB of 1.0 MiB</title>`,
		"<h2>Snapshot round trip</h2>", "<td>soak-2</td>", "20 s · index 1234", "30 s · index 1234, automatic", "GC collected 3 of the 3 overwritten chunks",
		"exact · &lt;b&gt;from the mirror&lt;/b&gt;",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(html, "<title>snapshot soak-1</title>") {
		t.Error("a snapshot event became a throughput marker")
	}
	sum.Records = sum.Records[:2]
	out.Reset()
	if err := Render(&out, sum); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Snapshot round trip") || strings.Contains(out.String(), "mirrors[].used") {
		t.Error("figures drawn for a run that recorded no snapshot")
	}
}
