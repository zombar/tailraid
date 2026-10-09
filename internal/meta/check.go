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
		if ino == RootIno {
			if in.Type != TDir {
				bad("root is not a directory")
			}
		} else {
			p, ok := st.Inodes[in.Parent]
			if !ok || p.Type != TDir {
				bad("inode %d: parent %d missing or not a directory", ino, in.Parent)
			} else if child, ok := p.Children[in.Name]; !ok || child != ino {
				bad("inode %d (%q): parent %d has no matching child entry", ino, in.Name, in.Parent)
			}
			if !reachesRoot(st, ino) {
				bad("inode %d: parent chain does not reach root", ino)
			}
			if in.Type == TDir {
				subdirs[in.Parent]++
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
	sort.Strings(v)
	return v
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
