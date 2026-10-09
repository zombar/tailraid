package meta

import (
	"sort"
	"time"
)

// MaxChunkSize bounds the volume's chunk size (I-67): the shard store, the RPC body limit
// and the flusher's buffers are all sized from it, and a k=1 chunk is stored whole.
const MaxChunkSize = 64 << 20

// MaxNameLen is the longest directory entry name in bytes (I-66), the NFS limit.
const MaxNameLen = 255

// MaxPathLen bounds the byte length of an entry's path from the root ("/a/b" is 4
// bytes), the PATH_MAX of Linux (I-115): Create, Mkdir and Rename refuse an entry whose
// own path would be longer with ErrNameTooLong. The bound is on the entry a command
// names; the descendants of a renamed directory are not re-measured.
const MaxPathLen = 4096

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
	Skewed      bool        // a host holds more than the host cap while a placement within it exists (docs/14 §7)
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
	readable := make([]bool, len(c.Placement))
	bad := make([]bool, len(c.Placement))
	for i, n := range c.Placement {
		if n == 0 {
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "missing"})
			bad[i] = true
			continue
		}
		nd, known := in.Nodes[n]
		if known && nd.Kind == KindDisk {
			// a disk is no more active than its host: draining or removing a host does
			// the same to its disks (docs/14 §1)
			if host, ok := in.Nodes[nd.Host]; ok && host.State > nd.State {
				nd.State = host.State
			}
		}
		live := in.Live.IsLive(n)
		present, checked := in.Present[i]
		isReadable := live && (!checked || present) && !in.Reported[i] && (!known || nd.State != Removed)
		switch {
		case known && nd.State == Draining:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "node draining", Readable: isReadable})
		case known && nd.State == Removed:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "node removed"})
		case !live && in.Live.DeadFor(n) >= in.FillAfter:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "node dead"})
		case in.Reported[i]:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "reported"})
		case checked && !present:
			h.Bad = append(h.Bad, SlotIssue{Idx: i, Reason: "absent"})
		}
		if len(h.Bad) > 0 && h.Bad[len(h.Bad)-1].Idx == i {
			bad[i] = true
		}
		readable[i] = isReadable
		if isReadable {
			h.Readable++
		}
	}
	h.skew(c, in, readable, bad)
	return h
}

// skew marks the slots a host holds above the host cap, highest slot first, when the
// live candidates admit a placement within it (docs/14 §7, I-89). Slots already bad do
// not count: they move anyway. With one target per host no host holds two slots, so this
// never fires on a cluster without disks.
func (h *ChunkHealth) skew(c Chunk, in HealthInput, readable, bad []bool) {
	hosts := map[NodeIdx][]int{} // host → its good slots
	over := false
	for i, n := range c.Placement {
		if n == 0 || bad[i] {
			continue
		}
		host := n
		if nd, ok := in.Nodes[n]; ok && nd.Kind == KindDisk {
			host = nd.Host
		}
		hosts[host] = append(hosts[host], i)
		over = over || len(hosts[host]) > 1
	}
	if !over {
		return
	}
	nodes := make([]Node, 0, len(in.Nodes))
	for idx := range in.Nodes {
		nodes = append(nodes, in.Nodes[idx])
	}
	targets := map[NodeIdx]int{} // domain → live candidate targets
	for _, c := range Candidates(nodes, in.Live.IsLive) {
		targets[c.domain()]++
	}
	n := len(c.Placement)
	limit := hostCap(n, len(targets))
	room := 0
	for _, t := range targets {
		room += min(t, limit)
	}
	if room < n {
		return // no placement within the cap exists now (L-194)
	}
	var issues []SlotIssue
	for _, slots := range hosts {
		for j := len(slots) - 1; j >= limit; j-- {
			issues = append(issues, SlotIssue{Idx: slots[j], Reason: "host over cap", Readable: readable[slots[j]]})
		}
	}
	if len(issues) == 0 {
		return
	}
	h.Skewed = true
	h.Bad = append(h.Bad, issues...)
	sort.Slice(h.Bad, func(a, b int) bool { return h.Bad[a].Idx < h.Bad[b].Idx })
}
