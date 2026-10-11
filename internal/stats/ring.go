package stats

import (
	"sort"
	"sync"
	"time"
)

// MaxLive is the most rows one node hands out over /v1/stats: the unsealed hour at the
// fastest sample interval (docs/17 §3, I-199).
const MaxLive = 720

// ring holds a writer's unsealed rows, oldest first.
type ring struct {
	mu   sync.Mutex
	rows []Row
}

func (r *ring) append(row Row) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, row)
	if n := len(r.rows); n > 1 && r.rows[n-1].T < r.rows[n-2].T {
		sort.SliceStable(r.rows, func(i, j int) bool { return r.rows[i].T < r.rows[j].T })
	}
}

// since returns at most limit rows newer than t, oldest first.
func (r *ring) since(t int64, limit int) []Row {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := sort.Search(len(r.rows), func(i int) bool { return r.rows[i].T > t })
	out := r.rows[i:]
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return append([]Row(nil), out...)
}

// cut removes and returns the rows before upTo.
func (r *ring) cut(upTo time.Time) []Row {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := sort.Search(len(r.rows), func(i int) bool { return r.rows[i].T >= upTo.Unix() })
	out := append([]Row(nil), r.rows[:i]...)
	r.rows = append([]Row(nil), r.rows[i:]...)
	return out
}

// giveBack restores rows a seal could not write (they stay unsealed, oldest first).
func (r *ring) giveBack(rows []Row) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(append([]Row(nil), rows...), r.rows...)
}

func (r *ring) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}
