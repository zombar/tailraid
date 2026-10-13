package node

import (
	"context"
	"errors"
	"path"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/mediatag"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/search"
)

// ScrapePage is the most items one scrape_plan reply carries.
const ScrapePage = 200

// scrapePlan answers scrape_plan (docs/18 §11): this node's plan for the items under
// op.Path, ScrapePage items (or op.Limit) at a time after op.Cursor, never forwarded. A
// show above op.Path is kept, since its seasons and episodes are matched through it; an
// album whose first track carries a picture has a cover.
func (a *adminOps) scrapePlan(ctx context.Context, op rpc.AdminOp) (any, error) {
	if a.search == nil {
		return nil, errors.New("scrape: this node has no search service")
	}
	under := path.Clean("/" + op.Path)
	limit := op.Limit
	if limit <= 0 || limit > ScrapePage {
		limit = ScrapePage
	}
	start := 0
	if op.Cursor != "" {
		n, err := strconv.Atoi(op.Cursor)
		if err != nil || n < 0 {
			return nil, errors.New("scrape: bad cursor")
		}
		start = n
	}
	items := a.search.ScrapePlan(ctx)
	pathOf := func(ino meta.Ino) (string, bool) {
		parts, err := a.node.FSM.PathOf(ino)
		return "/" + strings.Join(parts, "/"), err == nil
	}
	out, next := planPage(items, pathOf, a.hasPicture, under, start, limit)
	return map[string]any{"items": out, "next": next}, nil
}

// planPage is the page of items under under from start: at most limit, each with its
// folder's path, the cursor of the next page or "" when this is the last. A show above
// under is kept; an album whose first track carries a picture is dropped (its cover).
func planPage(items []search.ScrapeItem, pathOf func(meta.Ino) (string, bool), picture func(meta.Ino) bool, under string, start, limit int) ([]search.ScrapeItem, string) {
	out := []search.ScrapeItem{}
	for i := start; i < len(items); i++ {
		if len(out) == limit {
			return out, strconv.Itoa(i)
		}
		it := items[i]
		p, ok := pathOf(meta.Ino(it.Ino))
		dir, ok2 := pathOf(it.DirIno)
		if !ok || !ok2 {
			continue // gone since the walk
		}
		if !within(p, under) && (it.Kind != search.ScrapeShow || !within(under, p)) {
			continue
		}
		it.Dir = dir
		if it.Kind == search.ScrapeAlbum && it.Track != 0 && picture(it.Track) {
			continue // the cover is in the track (docs/18 §8)
		}
		out = append(out, it)
	}
	return out, ""
}

// within reports whether p is dir or under it.
func within(p, dir string) bool {
	return dir == "/" || p == dir || strings.HasPrefix(p, dir+"/")
}

// hasPicture reports whether the audio file ino carries an embedded picture.
func (a *adminOps) hasPicture(ino meta.Ino) bool {
	in, ok := a.node.FSM.GetInode(ino)
	parts, err := a.node.FSM.PathOf(ino)
	if !ok || err != nil {
		return false
	}
	f, err := a.fs.Open("/" + strings.Join(parts, "/"))
	if err != nil {
		return false
	}
	defer f.Close()
	_, pic := mediatag.Read(f, int64(in.Size)) //nolint:gosec // a file's size fits
	return pic != nil
}
