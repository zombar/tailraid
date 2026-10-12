package meta

import (
	"math"
	"slices"
	"sort"
)

// The trash (docs/02 §Apply rules, docs/06 §1): a delete moves an entry out of its
// parent's name table and keeps the inode, its subtree and every chunk reference it
// holds; only Purge drops references. A deleted root keeps Parent and Name as the place
// it came from and is indexed under that parent in State.Trash.

// protectedAncestors lists the protected directories on the parent chain of dir, dir
// itself included, nearest first.
func (a *applier) protectedAncestors(dir Ino) []Ino {
	var out []Ino
	for cur, steps := dir, 0; steps <= len(a.st.Inodes); steps++ {
		in, ok := a.st.Inodes[cur]
		if !ok {
			return out
		}
		if in.Type == TDir && in.Flags&FlagProtected != 0 {
			out = append(out, cur)
		}
		if cur == RootIno || in.Parent == 0 {
			return out
		}
		cur = in.Parent
	}
	return out
}

// subtreeStats counts the inodes and file bytes under in, in included, through the
// live name tables only: a nested deleted root is counted by its own delete.
func (st *State) subtreeStats(in *Inode) (inodes int, bytes uint64) {
	var walk func(*Inode)
	walk = func(x *Inode) {
		inodes++
		if x.Type == TFile {
			bytes += x.Size
		}
		for _, child := range x.Children {
			walk(st.Inodes[child])
		}
	}
	walk(in)
	return inodes, bytes
}

// trashEntry moves in, a child of p, into the trash at now: the name leaves p, the
// inode stays with everything under it, the stats move from live to trash, and the
// roots deleted from inside a directory earlier fold back into it (docs/02 §Trash).
func (a *applier) trashEntry(in, p *Inode, now int64) {
	st := a.st
	n, bytes := st.subtreeStats(in)
	a.stats.LogicalBytes -= bytes
	a.stats.TrashBytes += bytes
	a.stats.TrashInodes += n
	delete(p.Children, in.Name)
	in.DeletedAt = now
	in.Ctime = now
	st.indexTrash(p.Ino, in.Ino)
	if in.Type == TDir {
		p.Nlink--
		a.fold(in, now)
	}
	p.Mtime, p.Ctime = now, now
	p.Version++ // the name table changed (docs/01)
}

// fold puts the roots deleted from inside dir earlier back into it, in inode order: a
// tree removed file by file from a mount becomes one trash entry. A name already live
// in dir keeps its entry; the older root stays a root of its own. A fold that put a name
// back changes dir's name table, so its times and version move as for any entry added:
// an NFS client that cached the name's absence revalidates the directory (docs/07).
func (a *applier) fold(dir *Inode, now int64) {
	set := a.st.Trash[dir.Ino]
	if len(set) == 0 {
		return
	}
	inos := make([]Ino, 0, len(set))
	for ino := range set {
		inos = append(inos, ino)
	}
	slices.Sort(inos)
	changed := false
	for _, ino := range inos {
		r := a.st.Inodes[ino]
		if _, taken := dir.Children[r.Name]; taken {
			continue
		}
		dir.Children[r.Name] = ino
		r.DeletedAt = 0
		a.st.unindexTrash(dir.Ino, ino)
		if r.Type == TDir {
			dir.Nlink++
		}
		changed = true
	}
	if changed {
		dir.Mtime, dir.Ctime = now, now
		dir.Version++
	}
}

func (a *applier) trash(c *Trash) error {
	if c == nil {
		return ErrBadCommand
	}
	if err := validName(c.Name); err != nil {
		return err
	}
	st := a.st
	p, err := a.liveDir(c.Parent)
	if err != nil {
		return err
	}
	ino, ok := p.Children[c.Name]
	if !ok {
		return ErrNotExist
	}
	in := st.Inodes[ino]
	if st.lineage(ino).protected {
		return ErrProtected
	}
	if in.Type == TDir && len(in.Children) != 0 && !c.Recursive {
		return ErrNotEmpty
	}
	if IsEphemeral(in.Name) || st.lineage(ino).reserved {
		// an ephemeral name is unlinked, never trashed (I-157, L-208)
		batch := a.collect([]*Inode{in}, math.MaxInt)
		return a.deleteBatch(batch, c.Now)
	}
	a.trashEntry(in, p, a.clock(c.Now))
	return nil
}

func (a *applier) restore(c *Restore) error {
	if c == nil {
		return ErrBadCommand
	}
	st := a.st
	in, ok := st.Inodes[c.Ino]
	if !ok || in.DeletedAt == 0 {
		return ErrNotExist // only a root restores; an entry under one comes back with it
	}
	parent, name := c.NewParent, c.NewName
	if parent == 0 {
		parent = in.Parent
	}
	if name == "" {
		name = in.Name
	}
	np, err := a.liveDir(parent)
	if err != nil {
		return err
	}
	if err := validName(name); err != nil {
		return err
	}
	if a.pathLen(parent, name) > MaxPathLen {
		return ErrNameTooLong
	}
	if _, taken := np.Children[name]; taken {
		return ErrExist
	}
	now := a.clock(c.Now)
	st.unindexTrash(in.Parent, in.Ino)
	np.Children[name] = in.Ino
	in.Parent, in.Name, in.DeletedAt, in.Ctime = parent, name, 0, now
	if in.Type == TDir {
		np.Nlink++
	}
	np.Mtime, np.Ctime = now, now
	np.Version++ // the name table changed (docs/01)
	n, bytes := st.subtreeStats(in)
	a.stats.TrashInodes -= n
	a.stats.TrashBytes -= bytes
	a.stats.LogicalBytes += bytes
	return nil
}

// delItem is one inode the purge deletes and whether the trash stats held it (an
// ephemeral entry unlinked through trash was live).
type delItem struct {
	in      *Inode
	trashed bool
}

// collect lists, post-order, at most limit inodes under roots: children by name, then
// the roots deleted from inside a directory (its virtual children), then the directory
// itself, so a cut leaves every surviving parent valid and the root still due.
func (a *applier) collect(roots []*Inode, limit int) []delItem {
	var batch []delItem
	seen := map[Ino]bool{} // a nested root may be due on its own and reached through its ancestor
	var walk func(in *Inode, trashed bool) bool
	walk = func(in *Inode, trashed bool) bool {
		if seen[in.Ino] {
			return true
		}
		seen[in.Ino] = true
		trashed = trashed || in.DeletedAt != 0
		if in.Type == TDir {
			names := make([]string, 0, len(in.Children))
			for name := range in.Children {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if !walk(a.st.Inodes[in.Children[name]], trashed) {
					return false
				}
			}
			nested := make([]Ino, 0, len(a.st.Trash[in.Ino]))
			for ino := range a.st.Trash[in.Ino] {
				nested = append(nested, ino)
			}
			slices.Sort(nested)
			for _, ino := range nested {
				if !walk(a.st.Inodes[ino], true) {
					return false
				}
			}
		}
		if len(batch) >= limit {
			return false
		}
		batch = append(batch, delItem{in: in, trashed: trashed})
		return true
	}
	for _, r := range roots {
		if !walk(r, false) {
			break
		}
	}
	return batch
}

// deleteBatch drops every reference the batch's files hold (checked first, so a
// failure changes nothing), then removes the inodes from their parents and the table.
func (a *applier) deleteBatch(batch []delItem, cmdNow int64) error {
	st := a.st
	d := refDelta{}
	for _, it := range batch {
		if it.in.Type == TFile {
			d.add(it.in.Chunks, -1)
		}
	}
	if err := d.check(st); err != nil {
		return err
	}
	now := a.clock(cmdNow)
	d.commit(st, a.stats, now)
	for _, it := range batch {
		in := it.in
		switch {
		case in.DeletedAt != 0:
			st.unindexTrash(in.Parent, in.Ino)
		default:
			p := st.Inodes[in.Parent]
			delete(p.Children, in.Name)
			if in.Type == TDir {
				p.Nlink--
			}
			if !it.trashed { // a live parent lost a name (an ephemeral delete)
				p.Mtime, p.Ctime = now, now
				p.Version++
			}
		}
		if in.Type == TFile {
			if it.trashed {
				a.stats.TrashBytes -= in.Size
			} else {
				a.stats.LogicalBytes -= in.Size
			}
		}
		if it.trashed {
			a.stats.TrashInodes--
		}
		delete(st.Inodes, in.Ino)
	}
	return nil
}

func (a *applier) purge(c *Purge) error {
	if c == nil {
		return ErrBadCommand
	}
	st := a.st
	var roots []*Inode
	if c.Ino != 0 {
		in, ok := st.Inodes[c.Ino]
		if !ok || in.DeletedAt == 0 {
			return ErrNotExist
		}
		if st.lineage(c.Ino).protected {
			return ErrProtected // a user's "delete now" obeys the flag (docs/02 §Purge)
		}
		roots = []*Inode{in}
	} else {
		for _, set := range st.Trash {
			for ino := range set {
				if in := st.Inodes[ino]; in.DeletedAt < c.Before {
					roots = append(roots, in)
				}
			}
		}
		sort.Slice(roots, func(i, j int) bool {
			if roots[i].DeletedAt != roots[j].DeletedAt {
				return roots[i].DeletedAt < roots[j].DeletedAt
			}
			return roots[i].Ino < roots[j].Ino
		})
	}
	limit := int(c.Limit)
	if limit == 0 {
		limit = math.MaxInt
	}
	batch := a.collect(roots, limit)
	if len(batch) == 0 {
		return nil
	}
	return a.deleteBatch(batch, c.Now)
}
