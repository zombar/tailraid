package stats

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/chart"
)

func sampleRows() []Row {
	return []Row{
		{T: 1760184000, N: "jockey", M: map[string]float64{"cpu": 0.12, "load1": 0.8, "disk_free": 512e9}},
		{T: 1760184010, N: "jockey", M: map[string]float64{"cpu": 0.2}},
		{T: 1760184000, Term: 12, By: "jockey", C: &Cluster{
			Degraded: 37, Pending: 37, Stored: 6e10, Logical: 4.4e10, Chunks: 48211, RepairMoved: 5.2e6,
			Live:    map[string]int{"jockey": 1, "honker": 0, "node0": 2},
			Targets: []chart.Target{{Name: "jockey/1", Host: "jockey", Slots: 15020, Capacity: 5e11, Live: true, Active: true}},
		}},
	}
}

// A segment's rows survive the gzip JSON-lines codec unchanged, in order; an absent
// metric stays absent (never zero); the writer never emits NaN or Inf.
// Invariant: I-192
func TestRowCodec_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, sampleRows()); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 700 {
		t.Errorf("three rows take %d bytes gzipped; the budget assumes about 70 B a node row", buf.Len())
	}
	got, err := Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want := sampleRows()
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Fatalf("round trip differs:\n got %s\nwant %s", a, b)
	}
	if strings.Contains(string(a), `"load1":0,`) || strings.Contains(string(a), `"load1":0}`) || !strings.Contains(string(a), `"m":{"cpu":0.2}`) {
		t.Fatalf("an absent metric became zero: %s", a)
	}
	// NaN and Inf are dropped at encode time, the row kept
	var out bytes.Buffer
	if err := Encode(&out, []Row{{T: 1, N: "a", M: map[string]float64{"cpu": math.NaN(), "x": math.Inf(1), "ok": 1}}}); err != nil {
		t.Fatal(err)
	}
	rows, err := Decode(bytes.NewReader(out.Bytes()))
	if err != nil || len(rows) != 1 || len(rows[0].M) != 1 || rows[0].M["ok"] != 1 {
		t.Fatalf("NaN/Inf handling: %+v %v", rows, err)
	}
}

// Invariant: I-192
func TestRowCodec_Empty_And_Garbage(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if rows, err := Decode(bytes.NewReader(buf.Bytes())); err != nil || len(rows) != 0 {
		t.Fatalf("empty segment: %v %v", rows, err)
	}
	if _, err := Decode(bytes.NewReader([]byte("not gzip"))); err == nil {
		t.Fatal("garbage decoded")
	}
	if rows, err := Decode(bytes.NewReader(nil)); err != nil || len(rows) != 0 {
		t.Fatalf("a zero-length segment is empty, not an error: %v %v", rows, err)
	}
}

// Invariant: I-192
func FuzzRowCodec(f *testing.F) {
	var seed bytes.Buffer
	_ = Encode(&seed, sampleRows())
	f.Add(seed.Bytes())
	f.Add([]byte{})
	f.Add([]byte("{\"t\":1}\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		rows, err := Decode(bytes.NewReader(b))
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := Encode(&out, rows); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		again, err := Decode(bytes.NewReader(out.Bytes()))
		if err != nil || len(again) != len(rows) {
			t.Fatalf("decode(encode(rows)) differs: %v, %d vs %d rows", err, len(again), len(rows))
		}
	})
}
