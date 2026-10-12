package meta

import (
	"fmt"
	"slices"
	"sort"
)

// CheckInvariants verifies the structural invariants of a State (docs/02, I-27 and
// the lifecycle forbidden cells) and returns one message per violation. It is used
// by tests after every command and by `tailraid fsck`.
func CheckInvariants(st *State) []string {
	var v []string
	bad := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }

	// Inodes: parent/child agreement, chunk counts, nlink, NextIno.
	subdirs := map[Ino]int{}
	var maxIno Ino
	inos := make([]Ino, 0, len(st.Inodes))
	for ino := range st.Inodes {
		inos = append(inos, ino)
	}
	slices.Sort(inos)
	refs := map[Hash]int{}
	for _, ino := range inos {
		in := st.Inodes[ino]
		if ino > maxIno {
			maxIno = ino
		}
		if in.Ino != ino {
			bad("inode %d: Ino field is %d", ino, in.Ino)
		}
		if in.RewriteAt != 0 && (in.Type != TFile || in.RewriteAt > in.Version) {
			bad("inode %d: rewrite at version %d on a %d at version %d", ino, in.RewriteAt, in.Type, in.Version) // I-223
		}
		if in.Flags&^knownFlags != 0 || (in.Flags != 0 && in.Type != TDir) {
			bad("inode %d: flags %#x on a %d", ino, in.Flags, in.Type)
		}
		if ino == RootIno {
			if in.Type != TDir {
				bad("root is not a directory")
			}
			if in.DeletedAt != 0 {
				bad("root is in the trash")
			}
		} else {
			p, ok := st.Inodes[in.Parent]
			switch {
			case !ok || p.Type != TDir:
				bad("inode %d: parent %d missing or not a directory", ino, in.Parent)
			case in.DeletedAt != 0:
				// a deleted root: out of the name table, in the trash index (docs/01)
				if child, taken := p.Children[in.Name]; taken && child == ino {
					bad("inode %d (%q): deleted root still in parent %d's name table", ino, in.Name, in.Parent)
				}
				if _, indexed := st.Trash[in.Parent][ino]; !indexed {
					bad("inode %d: deleted root missing from the trash index", ino)
				}
			default:
				if child, ok := p.Children[in.Name]; !ok || child != ino {
					bad("inode %d (%q): parent %d has no matching child entry", ino, in.Name, in.Parent)
				}
				if in.Type == TDir {
					subdirs[in.Parent]++ // live subdirectories only: a deleted root left its parent's nlink
				}
			}
			if !reachesRoot(st, ino) {
				bad("inode %d: parent chain does not reach root", ino)
			}
		}
		switch in.Type {
		case TFile:
			if want := ChunkCount(in.Size, st.Vol.ChunkSize); st.Vol.ChunkSize != 0 && len(in.Chunks) != want {
				bad("inode %d: chunk count %d != ceil(size/chunk) %d", ino, len(in.Chunks), want)
			}
			for _, h := range in.Chunks {
				if h != HashZero {
					refs[h]++
				}
			}
			if in.Nlink != 1 {
				bad("inode %d: file nlink %d", ino, in.Nlink)
			}
		case TDir:
			for name, child := range in.Children {
				c, ok := st.Inodes[child]
				if !ok {
					bad("inode %d: child %q -> %d dangling", ino, name, child)
				} else if c.Parent != ino || c.Name != name {
					bad("inode %d: child %q -> %d has parent %d name %q", ino, name, child, c.Parent, c.Name)
				}
			}
		case TSymlink:
			if in.Nlink != 1 {
				bad("inode %d: symlink nlink %d", ino, in.Nlink)
			}
		default:
			bad("inode %d: bad type %d", ino, in.Type)
		}
	}
	for _, ino := range inos {
		in := st.Inodes[ino]
		if in.Type == TDir {
			if want := 2 + subdirs[ino]; int(in.Nlink) != want {
				bad("inode %d: dir nlink %d != %d", ino, in.Nlink, want)
			}
		}
	}
	if st.NextIno <= maxIno {
		bad("NextIno %d <= max ino %d", st.NextIno, maxIno)
	}

	// Chunks: refs, tombstones, placement, HashZero.
	for h, c := range st.Chunks {
		if h == HashZero {
			bad("chunk table contains HashZero")
			continue
		}
		if c.Hash != h {
			bad("chunk %x: Hash field mismatch", h[:4])
		}
		if int(c.Refs) != refs[h] {
			bad("chunk %x: refs %d != %d references", h[:4], c.Refs, refs[h])
		}
		_, tomb := st.Tombs[h]
		if c.Refs == 0 {
			if !tomb {
				bad("chunk %x: refs 0 but missing from Tombs", h[:4])
			}
			if c.TombstonedAt == 0 {
				bad("chunk %x: refs 0 with zero tombstone time", h[:4])
			}
		} else {
			if tomb {
				bad("chunk %x: live but present in Tombs", h[:4])
			}
			if c.TombstonedAt != 0 {
				bad("chunk %x: live with tombstone time %d", h[:4], c.TombstonedAt)
			}
		}
		if len(c.Sums) != 0 && len(c.Sums) != int(c.K) {
			bad("chunk %x: %d sums for k=%d", h[:4], len(c.Sums), c.K)
		}
		if !validPlacement(c.K, c.M, c.Placement) {
			bad("chunk %x: placement %v invalid for k=%d m=%d", h[:4], c.Placement, c.K, c.M)
		}
	}
	for h := range refs {
		if _, ok := st.Chunks[h]; !ok {
			bad("unknown chunk %x referenced by an inode", h[:4])
		}
	}
	for h := range st.Tombs {
		if _, ok := st.Chunks[h]; !ok {
			bad("Tombs entry %x has no chunk", h[:4])
		}
	}
	for parent, set := range st.Trash {
		for ino := range set {
			in, ok := st.Inodes[ino]
			switch {
			case !ok:
				bad("trash index: root %d under %d has no inode", ino, parent)
			case in.DeletedAt == 0 || in.Parent != parent:
				bad("trash index: root %d under %d is live or lies under %d", ino, parent, in.Parent)
			}
		}
		if len(set) == 0 {
			bad("trash index: empty set under %d", parent)
		}
	}
	checkSnaps(st, bad)
	sort.Strings(v)
	return v
}

// checkSnaps verifies the snapshot catalogue (docs/16 §5, I-179, I-184): every row on a
// live mirror row, with a valid name unique on its mirror, an ID below NextSnap, a known
// state, at most one sync per mirror; and no mirror is ever a placement slot.
func checkSnaps(st *State, bad func(string, ...any)) {
	type key struct {
		m    NodeIdx
		name string
	}
	names := map[key]SnapID{}
	syncing := map[NodeIdx]SnapID{}
	for id, sn := range st.Snaps {
		if sn.ID != id || id == 0 || id >= st.NextSnap {
			bad("snapshot %d: ID %d, NextSnap %d", id, sn.ID, st.NextSnap)
		}
		if m, ok := st.Nodes[sn.Mirror]; !ok || m.Kind != KindMirror || m.State == Removed {
			bad("snapshot %d: mirror %d is not a live mirror row", id, sn.Mirror)
		}
		if !validSnapName(sn.Name) {
			bad("snapshot %d: invalid name %q", id, sn.Name)
		}
		if other, dup := names[key{sn.Mirror, sn.Name}]; dup {
			bad("snapshots %d and %d share the name %q on mirror %d", other, id, sn.Name, sn.Mirror)
		}
		names[key{sn.Mirror, sn.Name}] = id
		if sn.State < SnapSyncing || sn.State > SnapDeleting {
			bad("snapshot %d: invalid state %d", id, sn.State)
		}
		if sn.State == SnapSyncing {
			if other, dup := syncing[sn.Mirror]; dup {
				bad("snapshots %d and %d both sync to mirror %d", other, id, sn.Mirror)
			}
			syncing[sn.Mirror] = id
		}
	}
	for h, ch := range st.Chunks {
		for _, n := range ch.Placement {
			if nd, ok := st.Nodes[n]; ok && nd.Kind == KindMirror {
				bad("chunk %x: a slot on mirror %d", h[:4], n)
			}
		}
	}
}

// CheckStats compares the stats the applier kept against a recount (I-153, I-156).
func CheckStats(st *State, kept Stats) []string {
	want := computeStats(st)
	kept.Inodes = want.Inodes
	if kept != want {
		return []string{fmt.Sprintf("stats drifted: kept %+v, recount %+v", kept, want)}
	}
	return nil
}

func reachesRoot(st *State, ino Ino) bool {
	seen := map[Ino]bool{}
	for cur := ino; ; {
		if cur == RootIno {
			return true
		}
		if seen[cur] {
			return false
		}
		seen[cur] = true
		in, ok := st.Inodes[cur]
		if !ok {
			return false
		}
		cur = in.Parent
	}
}
