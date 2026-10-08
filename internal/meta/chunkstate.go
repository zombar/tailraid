package meta

import "time"

// SmallChunkMax is the small-chunk threshold of docs/03: a chunk shorter than a full chunk
// and at most this many bytes is replicated (k=1, m=Vol.M) instead of striped.
const SmallChunkMax = 64 << 10

// IsSmallChunk reports whether a chunk of size bytes takes the replication policy.
func IsSmallChunk(size, chunkSize uint32) bool { return size < chunkSize && size <= SmallChunkMax }

// TargetPolicy is the (k, m) chunk c should have under the current volume policy: small
// chunks written with k=1 follow Vol.M only; everything else follows Vol.(K, M).
func TargetPolicy(c Chunk, vol Volume) (k, m uint8) {
	if c.K == 1 && IsSmallChunk(c.Size, vol.ChunkSize) {
		return 1, vol.M
	}
	return vol.K, vol.M
}

// Liveness is the prober view the state derivation needs (cluster.Prober).
type Liveness interface {
	IsLive(idx NodeIdx) bool
	DeadFor(idx NodeIdx) time.Duration
}

// SlotIssue is one placement slot that needs re-placing, with the reason of docs/06 §4.
// Readable marks a slot whose shard is still served by a live node (a draining holder):
// it counts toward readability and keeps its node until a replacement is placed (I-65).
type SlotIssue struct {
	Idx      int
	Reason   string
	Readable bool
}

// ChunkHealth is the derived chunk state of docs/11 §2 (I-56).
type ChunkHealth struct {
	Tombstoned  bool        // Refs == 0
	Bad         []SlotIssue // slots that need re-placing, in slot order
	Readable    int         // slots a read can use right now
	PolicyStale bool        // (K, M) differs from TargetPolicy: migration pending
}

// RepairPending reports whether any slot needs re-placing: the Degraded state of docs/11
// §2 that the repair scanner acts on (a dead holder counts only after fill_after).
func (h ChunkHealth) RepairPending() bool { return len(h.Bad) > 0 }

// UnderReplicated reports whether fewer than k+m copies are reachable right now, which is
// what an operator wants to know at once, before any hold-down: `status` counts these as
// degraded.
func (h ChunkHealth) UnderReplicated(k, m uint8) bool {
	return !h.Tombstoned && h.Readable < int(k)+int(m)
}

// Unreadable reports whether fewer than k slots can be read right now.
func (h ChunkHealth) Unreadable(k uint8) bool { return !h.Tombstoned && h.Readable < int(k) }

// State names the chunk state as `chunk describe` and `status` print it: tombstoned,
// unreadable, degraded (under-replicated now, repair pending, or awaiting migration) or
// committed.
func (h ChunkHealth) State(k, m uint8) string {
	switch {
	case h.Tombstoned:
		return "tombstoned"
	case h.Readable < int(k):
		return "unreadable"
	case h.UnderReplicated(k, m) || h.RepairPending() || h.PolicyStale:
		return "degraded"
	}
	return "committed"
}

// HealthInput is everything the derivation reads besides the chunk and the volume:
// the node table, liveness with the dead-node hold-down, the reader bad-slot reports
// and, when known (a HEAD was issued), whether each slot's shard is present.
type HealthInput struct {
	Nodes     map[NodeIdx]Node
	Live      Liveness
	FillAfter time.Duration
	Reported  map[int]bool // slot index → reported bad by a reader
	Present   map[int]bool // slot index → shard present; a slot absent from the map is unknown
}

// ChunkState derives the state of c from the FSM, the node table, liveness and (optional)
// shard presence, and nothing else (I-56). It is the one derivation shared by `status`,
// `check`, `chunk describe` and the repair scanner, so they always agree.
func ChunkState(c Chunk, vol Volume, in HealthInput) ChunkHealth {
	h := ChunkHealth{Tombstoned: c.Refs == 0}
	tk, tm := TargetPolicy(c, vol)
	h.PolicyStale = tk != c.K || tm != c.M
	for i, n := range c.Placement {
		if n == 0 {
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "missing"})
			continue
		}
		nd, known := in.Nodes[n]
		live := in.Live.IsLive(n)
		present, checked := in.Present[i]
		readable := live && (!checked || present) && !in.Reported[i] && (!known || nd.State != Removed)
		switch {
		case known && nd.State == Draining:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "node draining", Readable: readable})
		case known && nd.State == Removed:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "node removed"})
		case !live && in.Live.DeadFor(n) >= in.FillAfter:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "node dead"})
		case in.Reported[i]:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "reported"})
		case checked && !present:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "absent"})
		}
		if readable {
			h.Readable++
		}
	}
	return h
}
