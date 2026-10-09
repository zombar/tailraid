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
	Degraded       int                  // chunks with fewer than k+m reachable copies right now
	RepairPending  int                  // chunks with a slot to re-place (the repairer's view, after fill_after)
	Unreadable     int                  // chunks with fewer than k readable slots right now
	StalePolicy    int                  // chunks awaiting migration to the current (k, m)
	DomainSkew     int                  // chunks above the host cap that repair will move (docs/14 §7)
	DegradedHashes map[meta.Hash]bool   // degraded, repair-pending or unreadable
	Slots          map[meta.NodeIdx]int // placement slots per node
	Missing        map[meta.NodeIdx]int // slots per node whose node is not live
}

// ScanHealth derives every referenced chunk's state once.
func ScanHealth(st *meta.Store, live meta.Liveness, fillAfter time.Duration) VolumeHealth {
	vol := st.Volume()
	nodes := map[meta.NodeIdx]meta.Node{}
	all := st.Nodes()
	for i := range all {
		nodes[all[i].Idx] = all[i]
	}
	v := VolumeHealth{DegradedHashes: map[meta.Hash]bool{}, Slots: map[meta.NodeIdx]int{}, Missing: map[meta.NodeIdx]int{}}
	in := meta.HealthInput{Nodes: nodes, Live: live, FillAfter: fillAfter}
	st.ForEachChunk(func(c meta.Chunk) bool {
		if c.Refs == 0 {
			return true
		}
		for _, s := range c.Placement {
			if s != 0 {
				v.Slots[s]++
				if !live.IsLive(s) {
					v.Missing[s]++
				}
			}
		}
		h := meta.ChunkState(c, vol, in)
		if h.UnderReplicated(c.K, c.M) {
			v.Degraded++
			v.DegradedHashes[c.Hash] = true
		}
		if h.RepairPending() {
			v.RepairPending++
			v.DegradedHashes[c.Hash] = true
		}
		if h.Unreadable(c.K) {
			v.Unreadable++
			v.DegradedHashes[c.Hash] = true
		}
		if h.PolicyStale {
			v.StalePolicy++
		}
		if h.Skewed {
			v.DomainSkew++
		}
		return true
	})
	return v
}

// CheckReport is the `check` reply.
type CheckReport struct {
	Scan          repair.Stats `json:"scan"`
	Degraded      int          `json:"degraded_chunks"`       // fewer than k+m copies reachable now
	RepairPending int          `json:"repair_pending_chunks"` // a slot to re-place (after fill_after)
	Unreadable    int          `json:"unreadable_chunks"`
	Files         []string     `json:"files,omitempty"` // paths touching degraded/unreadable chunks
	OK            bool         `json:"ok"`
}

// FsckReport is the `fsck` reply.
type FsckReport struct {
	Violations   []string `json:"violations"`
	OrphanShards int      `json:"orphan_shards_on_this_node"`
	OK           bool     `json:"ok"`
}

// Check builds the `check` reply from the shared derivation plus the scan the caller ran
// (or the repairer's last pass). It is OK only when nothing is degraded or unreadable and
// the scan found nothing unrepairable or corrupt (L-106).
func Check(st *meta.Store, live meta.Liveness, fillAfter time.Duration, scan repair.Stats, files bool) CheckReport {
	v := ScanHealth(st, live, fillAfter)
	rep := CheckReport{Scan: scan, Degraded: v.Degraded, RepairPending: v.RepairPending, Unreadable: v.Unreadable}
	if files && len(v.DegradedHashes) > 0 {
		rep.Files = FilesReferencing(st, v.DegradedHashes)
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
