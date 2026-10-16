package admin

import (
	"sort"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
)

// VolumeHealth is the volume-wide roll-up of meta.ChunkState (docs/11 §2) that `status`
// and `check` share, so the two never disagree.
type VolumeHealth struct {
	Degraded      int                     // chunks with fewer than k+m reachable copies right now
	RepairPending int                     // chunks with a slot to re-place (the repairer's view, after fill_after)
	Unreadable    int                     // chunks with fewer than k readable slots right now
	StalePolicy   int                     // chunks awaiting migration to the current (k, m)
	DomainSkew    int                     // chunks above the host cap that repair will move (docs/14 §7)
	DiskSkew      int                     // slots away from home whose home is another disk of their host (docs/14 §7, I-172): the leader's last sweep, set by the caller
	BadClasses    []string                // the placement classes whose chunks are degraded, repair-pending or unreadable
	Slots         map[meta.NodeIdx]int    // placement slots per node
	Missing       map[meta.NodeIdx]int    // slots per node whose node is not live
	Bytes         map[meta.NodeIdx]uint64 // shard bytes per node, the sum of its slots' shard lengths (I-178)
}

// ScanHealth derives every referenced chunk's state from the placement classes: each
// class judged once by its representative and counted for all its chunks, so the cost is
// the number of classes and never the number of chunks (docs/12 §8, I-378). Each member's
// liveness is asked once (I-375).
func ScanHealth(st *meta.Store, live meta.Liveness, fillAfter time.Duration) VolumeHealth {
	vol := st.Volume()
	nodes := map[meta.NodeIdx]meta.Node{}
	all := st.Nodes()
	for i := range all {
		nodes[all[i].Idx] = all[i]
	}
	live = meta.FreezeLiveness(all, live) // each member asked once per walk, not once per slot (I-375)
	v := VolumeHealth{Slots: map[meta.NodeIdx]int{}, Missing: map[meta.NodeIdx]int{}, Bytes: map[meta.NodeIdx]uint64{}}
	in := meta.HealthInput{Nodes: nodes, Live: live, FillAfter: fillAfter}
	for _, cl := range st.Classes() {
		n := cl.Chunks
		for _, s := range cl.Placement {
			if s != 0 {
				v.Slots[s] += n
				v.Bytes[s] += cl.Shard
				if !live.IsLive(s) {
					v.Missing[s] += n
				}
			}
		}
		h := meta.ChunkState(cl.Representative(vol), vol, in)
		bad := false
		if h.UnderReplicated(cl.K, cl.M) {
			v.Degraded += n
			bad = true
		}
		if h.RepairPending() {
			v.RepairPending += n
			bad = true
		}
		if h.Unreadable(cl.K) {
			v.Unreadable += n
			bad = true
		}
		if h.PolicyStale {
			v.StalePolicy += n
		}
		if h.Skewed {
			v.DomainSkew += n
		}
		if bad {
			v.BadClasses = append(v.BadClasses, cl.Key)
		}
	}
	return v
}

// CheckReport is the `check` reply.
type CheckReport struct {
	Scan          repair.Stats     `json:"scan"`                  // the last completed pass
	InProgress    *repair.Progress `json:"in_progress,omitempty"` // the pass under way, if any (docs/12 §8)
	Degraded      int              `json:"degraded_chunks"`       // fewer than k+m copies reachable now
	RepairPending int              `json:"repair_pending_chunks"` // a slot to re-place (after fill_after)
	Unreadable    int              `json:"unreadable_chunks"`
	Files         []string         `json:"files,omitempty"`    // paths touching degraded/unreadable chunks
	Warnings      []string         `json:"warnings,omitempty"` // things to look at that do not fail the check (this host's mirrors, docs/12 §11)
	OK            bool             `json:"ok"`
}

// FsckReport is the `fsck` reply.
type FsckReport struct {
	Violations   []string `json:"violations"`
	OrphanShards int      `json:"orphan_shards_on_this_node"`
	// Orphans lists the first MaxListedOrphans of them by key, so a caller can look up
	// what left each one (the soak's phase 12 does, docs/09 §6).
	Orphans []string `json:"orphans,omitempty"`
	OK      bool     `json:"ok"`
}

// MaxListedOrphans bounds FsckReport.Orphans.
const MaxListedOrphans = 1000

// AddOrphan counts an orphan shard and lists its key while the list has room.
func (r *FsckReport) AddOrphan(key string) {
	r.OrphanShards++
	if len(r.Orphans) < MaxListedOrphans {
		r.Orphans = append(r.Orphans, key)
	}
}

// Check builds the `check` reply from the shared derivation plus the scan the caller ran
// (or the repairer's last pass). It is OK only when nothing is degraded or unreadable and
// the scan found nothing unrepairable or corrupt (L-106).
func Check(st *meta.Store, live meta.Liveness, fillAfter time.Duration, scan repair.Stats, prog *repair.Progress, files bool) CheckReport {
	v := ScanHealth(st, live, fillAfter)
	rep := CheckReport{Scan: scan, InProgress: prog, Degraded: v.Degraded, RepairPending: v.RepairPending, Unreadable: v.Unreadable}
	if files && len(v.BadClasses) > 0 {
		set := map[meta.Hash]bool{}
		for _, key := range v.BadClasses {
			for _, h := range st.ClassMembers(key) {
				set[h] = true
			}
		}
		rep.Files = FilesReferencing(st, set)
	}
	rep.OK = rep.Unreadable == 0 && rep.Degraded == 0 && rep.RepairPending == 0 && scan.Unrepairable == 0 && scan.ScrubBad == 0
	return rep
}

// FilesReferencing lists, sorted, every file path whose chunk list touches set.
func FilesReferencing(st *meta.Store, set map[meta.Hash]bool) []string {
	var out []string
	var walk func(ino meta.Ino, prefix string)
	walk = func(ino meta.Ino, prefix string) {
		ents, err := st.ReadDir(ino)
		if err != nil {
			return
		}
		for i := range ents {
			p := prefix + "/" + ents[i].Name
			switch ents[i].Type {
			case meta.TSymlink: // no chunks
			case meta.TDir:
				walk(ents[i].Ino, p)
			case meta.TFile:
				for _, h := range ents[i].Chunks {
					if set[h] {
						out = append(out, p)
						break
					}
				}
			}
		}
	}
	walk(meta.RootIno, "")
	sort.Strings(out)
	return out
}
