package admin

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
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
	Path       string `json:"path"`               // where it was deleted from, "/a/b/name"
	DeletedAt  string `json:"deleted_at"`         // RFC 3339 UTC
	PurgeAt    string `json:"purge_at,omitempty"` // when the retention purge takes it; absent with retention 0
	Restorable bool   `json:"restorable"`         // false while its original folder is itself deleted
}

// TrashList lists the deleted roots, every one or those under dir (nil for all),
// newest first. A root's path is the one it had; a root whose folder is itself in the
// trash is listed, not restorable in place. retention is trash.retention (0: no
// purge_at).
func TrashList(st *meta.Store, dir *meta.Ino, retention time.Duration) []TrashEntry {
	roots := trashRoots(st, dir)
	out := make([]TrashEntry, 0, len(roots))
	for i := range roots {
		out = append(out, trashEntry(st, &roots[i], retention))
	}
	return out
}

// TrashPage is TrashList a page at a time (I-234): the entries after the cursor (""
// for the first page), as many as fit in budget bytes of JSON and at least one, and the
// cursor of the next page ("" after the last). The cursor is the last entry's place in
// the trash's order (deletion time, then inode), so entries deleted or purged between
// pages neither repeat nor shift the rest.
func TrashPage(st *meta.Store, dir *meta.Ino, retention time.Duration, cursor string, budget int) ([]TrashEntry, string, error) {
	at, ino, err := parseTrashCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	roots := trashRoots(st, dir)
	var out []TrashEntry
	used := 2 // the JSON array's brackets
	for i := range roots {
		r := &roots[i]
		if cursor != "" && (r.DeletedAt > at || (r.DeletedAt == at && uint64(r.Ino) <= ino)) {
			continue // at or before the cursor in the trash's order
		}
		e := trashEntry(st, r, retention)
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, "", err
		}
		if len(out) > 0 && used+len(raw)+1 > budget {
			last := &roots[i-1]
			return out, fmt.Sprintf("%d.%d", last.DeletedAt, last.Ino), nil
		}
		used += len(raw) + 1
		out = append(out, e)
	}
	return out, "", nil
}

func parseTrashCursor(c string) (at int64, ino uint64, err error) {
	if c == "" {
		return 0, 0, nil
	}
	a, b, ok := strings.Cut(c, ".")
	if ok {
		if at, err = strconv.ParseInt(a, 10, 64); err == nil {
			if ino, err = strconv.ParseUint(b, 10, 64); err == nil {
				return at, ino, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("admin: trash cursor %q: want <deleted ns>.<ino>", c)
}

func trashRoots(st *meta.Store, dir *meta.Ino) []meta.Inode {
	if dir != nil {
		return st.TrashOf(*dir)
	}
	return st.Trash()
}

func trashEntry(st *meta.Store, r *meta.Inode, retention time.Duration) TrashEntry {
	e := TrashEntry{Ino: uint64(r.Ino), Name: r.Name, Size: r.Size, DeletedAt: time.Unix(0, r.DeletedAt).UTC().Format(time.RFC3339)}
	if retention > 0 {
		e.PurgeAt = time.Unix(0, r.DeletedAt).Add(retention).UTC().Format(time.RFC3339)
	}
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
	return e
}
