package soak

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The index is sampled and charted: a sample carries its files and pending count
// through JSON, and the report draws the chart only when some sample has them.
//
// Invariant: I-206, I-207
func TestReport_SearchChart(t *testing.T) {
	s := Sample{SearchDocs: 120, SearchPending: 7}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Sample
	if err := json.Unmarshal(b, &back); err != nil || back.SearchDocs != 120 || back.SearchPending != 7 {
		t.Fatalf("sample round trip: %+v %v", back, err)
	}
	started := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	render := func(samples ...*Sample) string {
		sum := Summary{Started: started, Duration: 10 * time.Second}
		for i, sm := range samples {
			sum.Records = append(sum.Records, Record{Kind: "sample", T: started.Add(time.Duration(i+1) * time.Second), S: sm})
		}
		var out bytes.Buffer
		if err := Render(&out, sum); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if !strings.Contains(render(&Sample{}, &s), "Search index") {
		t.Fatal("the report lacks the index chart")
	}
	if strings.Contains(render(&Sample{}), "Search index") {
		t.Fatal("the chart is drawn without a sample that has the measures")
	}
}
