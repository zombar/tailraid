package meta

import (
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// EventKind names a chunk lifecycle observation (docs/11 §2–§3 and the event ring).
type EventKind string

// Event kinds recorded by the flusher, reader, repairer and GC.
const (
	EvDedupHit     EventKind = "dedup_hit"      // L-33/L-34: hash already live, no upload
	EvFill         EventKind = "fill"           // L-176: dedup target had < k live slots, re-uploaded
	EvUploaded     EventKind = "uploaded"       // ≥ k shards acked
	EvPutFailed    EventKind = "put_failed"     // one slot's PUT failed or timed out
	EvDegraded     EventKind = "degraded"       // committed with missing slots
	EvCommitted    EventKind = "committed"      // the Commit referencing it applied
	EvUnknownChunk EventKind = "unknown_chunk"  // L-54: dedup target collected before Commit
	EvReadBadSlot  EventKind = "read_bad_slot"  // a reader got 404/corrupt from a slot
	EvReadBadChunk EventKind = "read_bad_chunk" // E-134: k shards decoded to the wrong hash; EIO, not cached
	EvRepaired     EventKind = "repaired"       // every slot restored
	EvPartial      EventKind = "partial_repair" // some slots restored, still degraded
	EvMigrated     EventKind = "migrated"       // re-encoded to the volume policy
	EvUnrepairable EventKind = "unrepairable"   // < k live good slots
	EvScrubOK      EventKind = "scrub_ok"       // L-140
	EvScrubBad     EventKind = "scrub_bad"      // a scrub found a corrupt shard
	EvCollected    EventKind = "collected"      // GC deleted its shards
)

// ChunkEvent is one entry of a chunk's history on one node.
type ChunkEvent struct {
	At     time.Time `json:"at"`
	Node   NodeIdx   `json:"node"`
	Kind   EventKind `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

// EventRing keeps the last ringLen events per chunk for the last ringChunks chunks touched
// on this node (dev.enabled only; a nil ring records nothing).
type EventRing struct {
	clk  clock.Clock
	self NodeIdx

	mu    sync.Mutex
	ring  map[Hash]*[]ChunkEvent
	order []Hash // least recently touched first
}

const (
	ringLen    = 64
	ringChunks = 4096
)

// NewEventRing returns a ring attributing events to self.
func NewEventRing(clk clock.Clock, self NodeIdx) *EventRing {
	return &EventRing{clk: clk, self: self, ring: map[Hash]*[]ChunkEvent{}}
}

// Record appends an event for h. Safe on a nil ring.
func (r *EventRing) Record(h Hash, kind EventKind, detail string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	evs, ok := r.ring[h]
	if !ok {
		if len(r.order) >= ringChunks {
			oldest := r.order[0]
			r.order = r.order[1:]
			delete(r.ring, oldest)
		}
		evs = new([]ChunkEvent)
		r.ring[h] = evs
	} else {
		for i, o := range r.order {
			if o == h {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
	}
	r.order = append(r.order, h)
	*evs = append(*evs, ChunkEvent{At: r.clk.Now(), Node: r.self, Kind: kind, Detail: detail})
	if len(*evs) > ringLen {
		*evs = (*evs)[len(*evs)-ringLen:]
	}
}

// Of returns a copy of h's events, oldest first. Nil on a nil ring.
func (r *EventRing) Of(h Hash) []ChunkEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	evs, ok := r.ring[h]
	if !ok {
		return []ChunkEvent{}
	}
	return append([]ChunkEvent(nil), *evs...)
}

// Kinds returns the kinds of h's events in order (test helper).
func (r *EventRing) Kinds(h Hash) []EventKind {
	evs := r.Of(h)
	out := make([]EventKind, len(evs))
	for i := range evs {
		out[i] = evs[i].Kind
	}
	return out
}
