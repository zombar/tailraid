package admin_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A trash of any size lists in pages that each stay within a byte budget (the admin
// reply a client reads is bounded), in the trash's order, every entry exactly once,
// also when entries are deleted and purged between pages: the cursor is a key of that
// order, not a position. A page always holds at least one entry, however long its path.
// Invariant: I-234
func TestTrashPage_EveryEntryOnceWithinBudget(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	const n = 300
	long := ""
	for len(long) < 200 {
		long += "a-rather-long-folder-name/"
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(fs.MkdirAll("/"+long, 0o755))
	trash := func(name string) {
		h, err := fs.Create("/" + long + name)
		must(err)
		must(h.Close())
		must(fs.Remove("/" + long + name))
	}
	for i := range n {
		f.Clk.Advance(1) // distinct deletion times, and some equal ones below
		trash(fmt.Sprintf("f%03d", i))
	}
	for i := range 20 { // twenty deleted in the same instant: the ino breaks the tie
		trash(fmt.Sprintf("same%02d", i))
	}
	want := admin.TrashList(f.FSM, nil, 0)

	const budget = 4 << 10
	seen := map[uint64]int{}
	var order []uint64
	var cursor string
	for pages := 0; ; pages++ {
		page, next, err := admin.TrashPage(f.FSM, nil, 0, cursor, budget)
		must(err)
		if len(page) == 0 && next != "" {
			t.Fatal("an empty page with a next cursor")
		}
		raw, _ := json.Marshal(page)
		if len(page) > 1 && len(raw) > budget {
			t.Fatalf("page %d is %d bytes, over the %d budget", pages, len(raw), budget)
		}
		for _, e := range page {
			seen[e.Ino]++
			order = append(order, e.Ino)
		}
		if pages == 2 { // the trash changes between pages
			trash("newest")                    // ahead of the cursor: not this listing's
			must(fs.Purge(meta.Ino(order[0]))) // already listed
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for i, e := range want {
		if seen[e.Ino] != 1 {
			t.Fatalf("entry %d (ino %d) listed %d times", i, e.Ino, seen[e.Ino])
		}
		if order[i] != e.Ino {
			t.Fatalf("position %d is ino %d, want %d (the trash's order)", i, order[i], e.Ino)
		}
	}
	if len(order) != len(want) {
		t.Fatalf("listed %d entries, want %d", len(order), len(want))
	}

	// one entry whose path alone is over the budget is still a page of its own
	page, _, err := admin.TrashPage(f.FSM, nil, 0, "", 10)
	must(err)
	if len(page) != 1 {
		t.Fatalf("a budget below one entry: %d entries, want 1", len(page))
	}
	if _, _, err := admin.TrashPage(f.FSM, nil, 0, "not-a-cursor", budget); err == nil {
		t.Fatal("a malformed cursor was accepted")
	}
}
