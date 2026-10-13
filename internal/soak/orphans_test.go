package soak

import (
	"slices"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// A shard the write path or repair leaves unplaced is an orphan the sweep removes after
// grace (L-190, docs/06): a PUT cut or failed that landed anyway, an upload whose commit
// never applied, a slot repair moved, a chunk GC collected. Phase 12 passes a new orphan
// only when its chunk's history on some node records one of those; an orphan nothing
// explains (a committed chunk whose slot never moved, a file that is not a shard) fails.
// Invariant: I-44
func TestOrphanReason_Table(t *testing.T) {
	ev := func(kinds ...meta.EventKind) []meta.ChunkEvent {
		var out []meta.ChunkEvent
		for _, k := range kinds {
			out = append(out, meta.ChunkEvent{Kind: k})
		}
		return out
	}
	cases := []struct {
		name     string
		events   []meta.ChunkEvent
		explains bool
	}{
		{"no history", nil, false},
		{"uploaded and committed", ev(meta.EvUploaded, meta.EvCommitted), false},
		{"dedup hit only", ev(meta.EvDedupHit, meta.EvCommitted), false},
		{"a PUT cut after k acks", ev(meta.EvPutFailed, meta.EvUploaded, meta.EvCommitted), true},
		{"uploaded, commit refused", ev(meta.EvUploaded), true},
		{"uploaded twice, committed once", ev(meta.EvUploaded, meta.EvUploaded, meta.EvCommitted), true},
		{"collected by GC", ev(meta.EvUploaded, meta.EvCommitted, meta.EvCollected), true},
		{"moved by repair", ev(meta.EvUploaded, meta.EvCommitted, meta.EvRepaired), true},
		{"migrated", ev(meta.EvUploaded, meta.EvCommitted, meta.EvMigrated), true},
		{"partially repaired", ev(meta.EvPartial), true},
		{"filled", ev(meta.EvFill, meta.EvCommitted), true},
	}
	for _, tc := range cases {
		if got := orphanReason(tc.events); (got != "") != tc.explains {
			t.Errorf("%s: reason %q, want explained=%v", tc.name, got, tc.explains)
		}
	}
}

// Only the orphans the phase added are judged, each by its chunk's history merged from
// every node; a key that does not parse as a shard is never explained.
// Invariant: I-44
func TestNewOrphans_JudgedByHistory(t *testing.T) {
	const (
		hCut   = "1111111111111111111111111111111111111111111111111111111111111111"
		hLeak  = "2222222222222222222222222222222222222222222222222222222222222222"
		hOld   = "3333333333333333333333333333333333333333333333333333333333333333"
		suffix = ".2-1.0"
	)
	history := map[string][]meta.ChunkEvent{
		hCut:  {{Kind: meta.EvUploaded}, {Kind: meta.EvCommitted}, {Kind: meta.EvPutFailed}},
		hLeak: {{Kind: meta.EvUploaded}, {Kind: meta.EvCommitted}},
	}
	before := []string{hOld + suffix}
	after := []string{hOld + suffix, hCut + suffix, hLeak + suffix, "junk"}
	explained, unexplained := judgeNewOrphans(before, after, func(h string) []meta.ChunkEvent { return history[h] })
	if len(explained) != 1 || explained[0].Key != hCut+suffix {
		t.Fatalf("explained %+v, want only the cut PUT's shard", explained)
	}
	want := []string{hLeak + suffix, "junk"}
	got := make([]string, 0, len(unexplained))
	for _, o := range unexplained {
		got = append(got, o.Key)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("unexplained %v, want %v", got, want)
	}
}
