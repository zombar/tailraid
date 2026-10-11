package soak

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The trash and the pinned cache are sampled and charted: a sample carries them through
// JSON, the report draws their chart only when some sample has them, and the totals name
// the most seen.
//
// Invariant: I-158, I-162
func TestReport_TrashCacheChart(t *testing.T) {
	s := Sample{TrashInodes: 3, TrashBytes: 4096, CachePinned: 1 << 20}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Sample
	if err := json.Unmarshal(b, &back); err != nil || back.TrashInodes != 3 || back.TrashBytes != 4096 || back.CachePinned != 1<<20 {
		t.Fatalf("sample round trip: %+v %v", back, err)
	}
	for _, name := range []string{"trash_inodes", "trash_bytes", "cache_pinned"} {
		if !strings.Contains(string(b), `"`+name+`"`) {
			t.Fatalf("sample JSON lacks %s: %s", name, b)
		}
	}
	started := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	with := Summary{Started: started, Duration: 10 * time.Second, Records: []Record{
		{Kind: "sample", T: started.Add(time.Second), S: &Sample{}},
		{Kind: "sample", T: started.Add(2 * time.Second), S: &s},
	}, Totals: Totals{MaxTrash: 3, MaxTrashB: 4096, MaxPinned: 1 << 20, Thumbs: 2, ThumbMax: 120 * time.Millisecond}}
	var out bytes.Buffer
	if err := Render(&out, with); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Trash and pinned cache", "3 entries, 4.0 KiB", "thumbnail round trips", "slowest 120ms"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report lacks %q", want)
		}
	}
	without := Summary{Started: started, Duration: 10 * time.Second, Records: []Record{{Kind: "sample", T: started.Add(time.Second), S: &Sample{}}}}
	out.Reset()
	if err := Render(&out, without); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Trash and pinned cache") {
		t.Fatal("the chart is drawn without a sample that has the measures")
	}
}
