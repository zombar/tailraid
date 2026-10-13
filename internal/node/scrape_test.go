package node

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
)

// A page holds at most limit items and names the next page's start; pages joined give
// every item once, in order, at every size around the limit.
// Invariant: I-299
func TestPlanPage_Sizes_Table(t *testing.T) {
	const limit = 3
	for _, n := range []int{0, 1, limit - 1, limit, limit + 1, 2 * limit} {
		items := make([]search.ScrapeItem, n)
		paths := map[meta.Ino]string{}
		for i := range items {
			ino := meta.Ino(100 + i)
			items[i] = search.ScrapeItem{Kind: search.ScrapeMovie, Ino: uint64(ino), DirIno: ino}
			paths[ino] = fmt.Sprintf("/films/f%d", i)
		}
		pathOf := func(ino meta.Ino) (string, bool) { p, ok := paths[ino]; return p, ok }
		var got []uint64
		start, pages := 0, 0
		for {
			page, next := planPage(items, pathOf, func(meta.Ino) bool { return false }, "/", start, limit)
			pages++
			if len(page) > limit {
				t.Fatalf("n=%d: a page of %d", n, len(page))
			}
			for _, it := range page {
				got = append(got, it.Ino)
				if it.Dir != paths[meta.Ino(it.Ino)] {
					t.Errorf("n=%d: dir %q", n, it.Dir)
				}
			}
			if next == "" {
				break
			}
			if _, err := fmt.Sscan(next, &start); err != nil {
				t.Fatal(err)
			}
		}
		if len(got) != n || pages != max(1, (n+limit-1)/limit) {
			t.Errorf("n=%d: %d items in %d pages", n, len(got), pages)
		}
	}
}

// Only items under the path are listed, the show above it kept; an album whose track
// carries a picture and an item gone since the walk are not.
// Invariant: I-299
func TestPlanPage_Under(t *testing.T) {
	paths := map[meta.Ino]string{1: "/tv/Show", 2: "/tv/Show/Season 01", 3: "/tv/Show/Season 01/e1.mkv", 4: "/tv/Other", 5: "/music/A/B", 6: "/music/A/B/01.mp3", 7: "/music/A/C"}
	items := []search.ScrapeItem{
		{Kind: search.ScrapeShow, Ino: 1, DirIno: 1},
		{Kind: search.ScrapeEpisode, Ino: 3, DirIno: 2},
		{Kind: search.ScrapeShow, Ino: 4, DirIno: 4},
		{Kind: search.ScrapeAlbum, Ino: 5, DirIno: 5, Track: 6},
		{Kind: search.ScrapeAlbum, Ino: 7, DirIno: 7},
		{Kind: search.ScrapeMovie, Ino: 99, DirIno: 99}, // gone
	}
	pathOf := func(ino meta.Ino) (string, bool) { p, ok := paths[ino]; return p, ok }
	picture := func(ino meta.Ino) bool { return ino == 6 }
	for _, tc := range []struct {
		under string
		want  []uint64
	}{
		{"/", []uint64{1, 3, 4, 7}},
		{"/tv/Show/Season 01", []uint64{1, 3}},
		{"/tv/Other", []uint64{4}},
		{"/music", []uint64{7}},
		{"/films", nil},
	} {
		page, _ := planPage(items, pathOf, picture, tc.under, 0, ScrapePage)
		var got []uint64
		for _, it := range page {
			got = append(got, it.Ino)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("under %s: %v, want %v", tc.under, got, tc.want)
		}
	}
}
