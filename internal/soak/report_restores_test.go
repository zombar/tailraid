package soak

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Phase 32's figures: the report draws one line of bytes done per restore job with the
// phase's marks (the mirror unplugged and back, the host restarted) and the jobs table;
// a run that followed no job draws none of it.
//
// Invariant: I-365
func TestReport_RestoreFigures(t *testing.T) {
	started := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return started.Add(time.Duration(sec) * time.Second) }
	sample := func(sec int, id, label, b string) Record {
		return Record{Kind: "event", Name: restoreSample, T: at(sec), Text: id, Attrs: map[string]string{"label": label, "state": "running", "bytes": b, "total": "1048576"}}
	}
	sum := Summary{Started: started, Duration: 120 * time.Second, Records: []Record{
		sample(10, "2-1", "first", "0"),
		sample(20, "2-1", "first", "1048576"),
		{Kind: "event", Name: restoreMark, T: at(30), Text: "mirror unplugged"},
		{Kind: "event", Name: restoreMark, T: at(40), Text: "host restarted"},
		sample(50, "2-2", "waited and resumed", "524288"),
		{Kind: "event", Name: restoreRow, T: at(60), Text: "2-2", Attrs: map[string]string{
			"label": "waited and resumed", "dst": "/soak/restored-b/restore-jobs", "state": "done", "files": "242 of 242", "bytes": "1.0 MiB", "existing": "0", "resumed": "0", "took": "41s",
		}},
	}}
	var out bytes.Buffer
	if err := Render(&out, sum); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"Restore jobs — bytes done", ">2-1 (first)<", ">2-2 (waited and resumed)<", ">mirror unplugged<", ">host restarted<",
		"<h2>Restores</h2>", "<td>/soak/restored-b/restore-jobs</td>", "<td>waited and resumed</td>", "242 of 242",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	out.Reset()
	if err := Render(&out, Summary{Started: started, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<h2>Restores</h2>") || strings.Contains(out.String(), "Restore jobs — bytes done") {
		t.Fatal("a run with no restore job draws the section")
	}
}
