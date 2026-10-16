package meta

import "maps"

// libraries is the applier's set of the directories whose flags carry a library kind
// (docs/18 §9, I-384): what Store.Libraries reads instead of every inode. Kept by the
// SetAttr that sets flags and the deletes, rebuilt on Restore and never encoded. A
// trashed library stays in it; Libraries leaves it out by its lineage.
type libraries struct{ dirs map[Ino]struct{} }

func newLibraries() *libraries { return &libraries{dirs: map[Ino]struct{}{}} }

func buildLibraries(st *State) *libraries {
	l := newLibraries()
	for _, in := range st.Inodes {
		l.set(in)
	}
	return l
}

// set records in's flags: in the set while they carry a library kind.
func (l *libraries) set(in *Inode) {
	if l == nil {
		return
	}
	if in.Type == TDir && in.Library() != LibraryNone {
		l.dirs[in.Ino] = struct{}{}
	} else {
		delete(l.dirs, in.Ino)
	}
}

// drop forgets an inode deleted from the table.
func (l *libraries) drop(ino Ino) {
	if l != nil {
		delete(l.dirs, ino)
	}
}

func librariesDrift(want, kept *libraries) []string {
	if kept == nil {
		return []string{"no library set kept"}
	}
	if !maps.Equal(want.dirs, kept.dirs) {
		return []string{"library set drifted"}
	}
	return nil
}
