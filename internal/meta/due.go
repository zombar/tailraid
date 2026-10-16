package meta

import (
	"bytes"
	"container/heap"
	"fmt"
	"maps"
	"math"
)

// The applier's due queues (docs/02 §Apply rules, I-381, I-382): GCDelete and
// Purge{Before} take their batch from these instead of walking Tombs or Trash. Like the
// placement classes they are kept with every apply, rebuilt on Restore and never encoded.
type dueIndex struct {
	waiting *queue[Hash] // tombstones not yet due at the last GCDelete, by (TombstonedAt, Hash)
	due     *queue[Hash] // tombstones a GCDelete found due, by Hash: the order victims go in
	roots   *queue[Ino]  // the deleted roots, by (DeletedAt, Ino): the order Purge takes them in
}

func newDueIndex() *dueIndex {
	return &dueIndex{
		waiting: newQueue(func(a, b dueEnt[Hash]) bool {
			return a.at < b.at || a.at == b.at && bytes.Compare(a.id[:], b.id[:]) < 0
		}),
		due:   newQueue(func(a, b dueEnt[Hash]) bool { return bytes.Compare(a.id[:], b.id[:]) < 0 }),
		roots: newQueue(func(a, b dueEnt[Ino]) bool { return a.at < b.at || a.at == b.at && a.id < b.id }),
	}
}

func buildDue(st *State) *dueIndex {
	d := newDueIndex()
	for h := range st.Tombs {
		d.tomb(h, st.Chunks[h].TombstonedAt)
	}
	for _, set := range st.Trash {
		for ino := range set {
			d.roots.add(dueEnt[Ino]{at: st.Inodes[ino].DeletedAt, id: ino})
		}
	}
	return d
}

// tomb queues a chunk that reached Refs 0 at at; a chunk already queued keeps its place.
func (d *dueIndex) tomb(h Hash, at int64) {
	if d == nil || d.waiting.has(h) || d.due.has(h) {
		return
	}
	d.waiting.add(dueEnt[Hash]{at: at, id: h})
}

// untomb drops a chunk that is referenced again.
func (d *dueIndex) untomb(h Hash) {
	if d != nil && !d.waiting.remove(h) {
		d.due.remove(h)
	}
}

// takeTombs dequeues the lowest hashes among the tombstones with at < before, at most
// limit (0 is every one): the victims of GCDelete's walk, in its order (I-381). A queued
// due entry this before does not reach (a sync hold, a lower Before) waits again.
func (d *dueIndex) takeTombs(before int64, limit int) []Hash {
	if limit == 0 {
		limit = math.MaxInt
	}
	for e, ok := d.waiting.front(); ok && e.at < before; e, ok = d.waiting.front() {
		d.due.add(d.waiting.take())
	}
	var out []Hash
	var later []dueEnt[Hash]
	for d.due.Len() > 0 && len(out) < limit {
		e := d.due.take()
		if e.at < before {
			out = append(out, e.id)
		} else {
			later = append(later, e)
		}
	}
	for _, e := range later {
		d.waiting.add(e)
	}
	return out
}

// dueDrift compares the queues the applier kept against a rebuild (I-381, I-382).
func dueDrift(want, kept *dueIndex) []string {
	if kept == nil {
		return []string{"no due queues kept"}
	}
	tombs := func(d *dueIndex) map[Hash]int64 {
		m := map[Hash]int64{}
		for _, q := range []*queue[Hash]{d.waiting, d.due} {
			for _, e := range q.ents {
				m[e.id] = e.at
			}
		}
		return m
	}
	if w, k := tombs(want), tombs(kept); !maps.Equal(w, k) || len(w) != kept.waiting.Len()+kept.due.Len() {
		return []string{fmt.Sprintf("tombstone queues drifted: kept %d, recount %d", len(k), len(w))}
	}
	roots := func(d *dueIndex) map[Ino]int64 {
		m := map[Ino]int64{}
		for _, e := range d.roots.ents {
			m[e.id] = e.at
		}
		return m
	}
	if w, k := roots(want), roots(kept); !maps.Equal(w, k) {
		return []string{fmt.Sprintf("root queue drifted: kept %d, recount %d", len(k), len(w))}
	}
	return nil
}

// dueEnt is one queued entry: the time it ages from and what it is.
type dueEnt[ID comparable] struct {
	at int64
	id ID
}

// queue is a min-heap of entries that also removes any entry by its identity, so an
// entry leaves at the apply that changes it and the front is always exact.
type queue[ID comparable] struct {
	less func(a, b dueEnt[ID]) bool
	ents []dueEnt[ID]
	pos  map[ID]int
}

func newQueue[ID comparable](less func(a, b dueEnt[ID]) bool) *queue[ID] {
	return &queue[ID]{less: less, pos: map[ID]int{}}
}

func (q *queue[ID]) has(id ID) bool { _, ok := q.pos[id]; return ok }

func (q *queue[ID]) add(e dueEnt[ID]) { heap.Push(q, e) }

func (q *queue[ID]) remove(id ID) bool {
	i, ok := q.pos[id]
	if ok {
		heap.Remove(q, i)
	}
	return ok
}

func (q *queue[ID]) front() (dueEnt[ID], bool) {
	if len(q.ents) == 0 {
		return dueEnt[ID]{}, false
	}
	return q.ents[0], true
}

func (q *queue[ID]) take() dueEnt[ID] { return heap.Pop(q).(dueEnt[ID]) }

// heap.Interface; use add, remove and take.
func (q *queue[ID]) Len() int           { return len(q.ents) }
func (q *queue[ID]) Less(i, j int) bool { return q.less(q.ents[i], q.ents[j]) }
func (q *queue[ID]) Swap(i, j int) {
	q.ents[i], q.ents[j] = q.ents[j], q.ents[i]
	q.pos[q.ents[i].id], q.pos[q.ents[j].id] = i, j
}

func (q *queue[ID]) Push(x any) {
	e := x.(dueEnt[ID])
	q.pos[e.id] = len(q.ents)
	q.ents = append(q.ents, e)
}

func (q *queue[ID]) Pop() any {
	e := q.ents[len(q.ents)-1]
	q.ents = q.ents[:len(q.ents)-1]
	delete(q.pos, e.id)
	return e
}
