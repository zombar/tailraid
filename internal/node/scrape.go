package node

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	var remembered map[string]bool
	if !op.All { // --again: the items answered "no match" are asked about again (I-394)
		remembered = rememberedSkips(a.fs)
	}
	pathOf := func(ino meta.Ino) (string, bool) {
		parts, err := a.node.FSM.PathOf(ino)
		return "/" + strings.Join(parts, "/"), err == nil
	}
	out, next := planPage(items, pathOf, a.hasPicture, remembered, under, start, limit)
	return map[string]any{"items": out, "next": next}, nil
}

// planPage is the page of items under under from start: at most limit, each with its
// folder's path, the cursor of the next page or "" when this is the last. A show above
// under is kept; an album whose first track carries a picture is dropped (its cover), and
// so is an item remembered as "no match" (I-394).
func planPage(items []search.ScrapeItem, pathOf func(meta.Ino) (string, bool), picture func(meta.Ino) bool, remembered map[string]bool, under string, start, limit int) ([]search.ScrapeItem, string) {
	out := []search.ScrapeItem{}
	for i := start; i < len(items); i++ {
		if len(out) == limit {
			return out, strconv.Itoa(i)
		}
		it := items[i]
		if remembered[skipKey(it)] {
			continue
		}
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

// skipDir holds one empty file per item answered "no match" (docs/18 §11, I-394).
var skipDir = "/" + meta.ReservedDir + "/scrape/skips"

// skipKey is an item's marker name: its kind and inode, so a rename keeps the answer and
// a file made anew is asked about again.
func skipKey(it search.ScrapeItem) string { return it.Kind + "-" + strconv.FormatUint(it.Ino, 10) }

// scrapeKinds are the kinds a plan item has.
var scrapeKinds = map[string]bool{search.ScrapeMovie: true, search.ScrapeShow: true, search.ScrapeSeason: true, search.ScrapeEpisode: true, search.ScrapeAlbum: true, search.ScrapeGame: true}

// scrapeSkip answers scrape_skip: the item op.Ino of kind op.Kind is remembered as "no
// match" (I-394). The caller's Write on op.Path was checked; the marker is the node's.
func (a *adminOps) scrapeSkip(_ context.Context, op rpc.AdminOp) (any, error) {
	if !scrapeKinds[op.Kind] || op.Ino == 0 {
		return nil, errors.New("scrape: scrape_skip needs an item's kind and inode")
	}
	if err := a.fs.MkdirAll(skipDir, 0o755); err != nil {
		return nil, fmt.Errorf("scrape: %w", err)
	}
	name := path.Join(skipDir, skipKey(search.ScrapeItem{Kind: op.Kind, Ino: op.Ino}))
	f, err := a.fs.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	switch {
	case errors.Is(err, os.ErrExist):
		return map[string]any{"remembered": true}, nil // answered before: nothing changes
	case err != nil:
		return nil, fmt.Errorf("scrape: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("scrape: %w", err)
	}
	return map[string]any{"remembered": true}, nil
}

// dirReader lists a directory, as the vfs does.
type dirReader interface {
	ReadDir(string) ([]os.FileInfo, error)
}

// rememberedSkips is the marker names under skipDir; none when it cannot be listed.
func rememberedSkips(fs dirReader) map[string]bool {
	fis, err := fs.ReadDir(skipDir)
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(fis))
	for _, fi := range fis {
		out[fi.Name()] = true
	}
	return out
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
