package meta

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

func TestEventRing_BoundedAndNilSafe(t *testing.T) {
	var nilRing *EventRing
	nilRing.Record(Hash{1}, EvCommitted, "")
	if nilRing.Of(Hash{1}) != nil || nilRing.Kinds(Hash{1}) == nil {
		t.Fatal("nil ring must record nothing and answer empty")
	}
	r := NewEventRing(clock.NewFake(time.Unix(0, 0)), 3)
	var h Hash
	h[0] = 7
	for i := 0; i < ringLen+10; i++ {
		r.Record(h, EvUploaded, "")
	}
	r.Record(h, EvCommitted, "last")
	evs := r.Of(h)
	if len(evs) != ringLen || evs[len(evs)-1].Kind != EvCommitted || evs[len(evs)-1].Node != 3 {
		t.Fatalf("ring holds %d events, last %+v", len(evs), evs[len(evs)-1])
	}
	// chunk cap: the least recently touched hash is dropped first
	for i := 0; i < ringChunks; i++ {
		var o Hash
		o[0], o[1], o[2] = 1, byte(i>>8), byte(i)
		r.Record(o, EvScrubOK, "")
	}
	if len(r.Of(h)) != 0 {
		t.Fatal("oldest hash should have been evicted")
	}
	var keep Hash
	keep[0], keep[1], keep[2] = 1, 0, 0
	if len(r.Of(keep)) != 1 {
		t.Fatal("a hash within the cap must be kept")
	}
	if len(r.ring) != ringChunks || len(r.order) != ringChunks {
		t.Fatalf("ring tracks %d/%d hashes", len(r.ring), len(r.order))
	}
}
