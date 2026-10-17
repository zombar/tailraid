package soak

import (
	"reflect"
	"strings"
	"testing"
)

// Phase 35's media queries are a table of the report: each query with every gateway's
// answer time, in run order.
// Invariant: I-412
func TestReport_MediaQueries(t *testing.T) {
	s := Summary{Records: []Record{
		{Kind: "event", Name: watchQuery, Text: `actor:"a b"`, Attrs: map[string]string{"n2": "9", "n1": "12"}},
		{Kind: "event", Name: "other", Text: "x"},
		{Kind: "event", Name: watchQuery, Text: "heist", Attrs: map[string]string{"n1": "3"}},
	}}
	got := queryRows(s)
	want := []queryView{{Query: `actor:"a b"`, Times: "n1 12 ms · n2 9 ms"}, {Query: "heist", Times: "n1 3 ms"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	var b strings.Builder
	if err := Render(&b, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "<h2>Media queries</h2>") || !strings.Contains(b.String(), "n1 12 ms · n2 9 ms") {
		t.Fatal("the report has no media query table")
	}
}
