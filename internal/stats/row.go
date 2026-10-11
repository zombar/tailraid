// Package stats keeps the cluster's time series inside the volume (docs/17): every node
// samples itself and the leader the cluster, the rows are gzip JSON-lines segments under
// /.tailraid/stats/<tier>/<name>/, rolled up by the writer and pruned by the leader, and
// the Stats page draws them (docs/13).
package stats

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/zombar/tailraid/internal/chart"
)

// Row is one sample: a node row (N and M) or a cluster row (Term, By and C).
type Row struct {
	T    int64              `json:"t"`              // Unix seconds, the sampler's clock
	N    string             `json:"n,omitempty"`    // the member name (node rows)
	M    map[string]float64 `json:"m,omitempty"`    // the node metrics of docs/17 §1; an absent vital is absent
	Term uint64             `json:"term,omitempty"` // the Raft term the writer held (cluster rows)
	By   string             `json:"by,omitempty"`   // the leader that wrote it
	C    *Cluster           `json:"c,omitempty"`
}

// Cluster is the leader's view: the volume counters, the repairer's backlog and pass,
// every device's liveness and every target's slots, for the disk-spread chart.
type Cluster struct {
	Degraded    float64        `json:"degraded"`
	Pending     float64        `json:"pending"`
	Stale       float64        `json:"stale"`
	Unreadable  float64        `json:"unreadable"`
	Stored      float64        `json:"stored"`
	Logical     float64        `json:"logical"`
	Chunks      float64        `json:"chunks"`
	DomainSkew  float64        `json:"domain_skew"`
	DiskSkew    float64        `json:"disk_skew"`
	RepairMoved float64        `json:"repair_moved"` // bytes/s of the running pass
	Live        map[string]int `json:"live"`         // per device: 0 dead, 1 live, 2 suspect
	Targets     []chart.Target `json:"targets"`
}

// Encode writes rows as gzip-compressed JSON lines. A NaN or Inf metric is dropped from
// its row (an absent vital is absent, never a bad number).
func Encode(w io.Writer, rows []Row) error {
	gz := gzip.NewWriter(w)
	enc := json.NewEncoder(gz)
	for _, r := range rows {
		if hasBad(r.M) {
			m := make(map[string]float64, len(r.M))
			for k, v := range r.M {
				if !math.IsNaN(v) && !math.IsInf(v, 0) {
					m[k] = v
				}
			}
			r.M = m
		}
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("encode row: %w", err)
		}
	}
	return gz.Close()
}

func hasBad(m map[string]float64) bool {
	for _, v := range m {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

// maxLine bounds one row: a cluster row with hundreds of targets is still far under it.
const maxLine = 1 << 20

// Decode reads a segment. A zero-length segment (a name published before its flush)
// is empty, not an error.
func Decode(r io.Reader) ([]Row, error) {
	gz, err := gzip.NewReader(r)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("segment: %w", err)
	}
	var rows []Row
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row Row
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("segment row: %w", err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("segment: %w", err)
	}
	return rows, nil
}
