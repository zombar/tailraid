package admin

import (
	"path"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// TrashEntry is one deleted root as `tailraid trash ls` and the browser UI list it
// (docs/12 §10, docs/13 §3).
type TrashEntry struct {
	Ino        uint64 `json:"ino"`
	Name       string `json:"name"`
	Type       string `json:"type"` // file | dir | symlink
	Size       uint64 `json:"size"`
	Path       string `json:"path"`       // where it was deleted from, "/a/b/name"
	DeletedAt  string `json:"deleted_at"` // RFC 3339 UTC
	Restorable bool   `json:"restorable"` // false while its original folder is itself deleted
}

// TrashList lists the deleted roots, every one or those under dir (nil for all),
// newest first. A root's path is the one it had; a root whose folder is itself in the
// trash is listed, not restorable in place.
func TrashList(st *meta.Store, dir *meta.Ino) []TrashEntry {
	var roots []meta.Inode
	if dir != nil {
		roots = st.TrashOf(*dir)
	} else {
		roots = st.Trash()
	}
	out := make([]TrashEntry, 0, len(roots))
	for i := range roots {
		r := &roots[i]
		e := TrashEntry{Ino: uint64(r.Ino), Name: r.Name, Size: r.Size, DeletedAt: time.Unix(0, r.DeletedAt).UTC().Format(time.RFC3339)}
		switch r.Type {
		case meta.TDir:
			e.Type = "dir"
		case meta.TSymlink:
			e.Type = "symlink"
		default:
			e.Type = "file"
		}
		if parts, err := st.PathOf(r.Ino); err == nil {
			e.Path = "/" + path.Join(parts...)
		}
		_, e.Restorable = st.GetInode(r.Parent) // the parent is live
		out = append(out, e)
	}
	return out
}
