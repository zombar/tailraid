package nfsgw_test

import (
	"fmt"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
)

// A listing paged while its directory changes goes on from the listing it started with:
// macOS takes NFS3ERR_BAD_COOKIE as the end of the directory (and Linux after one retry),
// so a refusal showed a Finder copy, an ls, an rsync a truncated folder. Every name the
// first page's listing held and that still exists is listed exactly once, however many
// other directories are listed in between (up to the cache's bound); one removed
// meanwhile is left out.
// Invariant: I-232
func TestNFS_ReadDir_ChangedMidListing_NoBadCookie(t *testing.T) {
	for _, others := range []int{0, 40} {
		t.Run(fmt.Sprintf("%d other listings in between", others), func(t *testing.T) {
			g := startGateway(t)
			if _, err := g.tgt.Mkdir("/pg", 0o755); err != nil {
				t.Fatal(err)
			}
			const n = 150
			before := map[string]bool{}
			for i := range n {
				name := fmt.Sprintf("e%03d", i)
				if _, err := g.tgt.Create("/pg/"+name, 0o644); err != nil {
					t.Fatal(err)
				}
				before[name] = true
			}
			for i := range others {
				if _, err := g.tgt.Mkdir(fmt.Sprintf("/o%02d", i), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, fh, err := g.tgt.Lookup("/pg")
			must(t, err)
			status, page, verf, eof := readDirPage(t, g, fh, 0, 0)
			if status != nfsc.NFS3Ok || eof {
				t.Fatalf("first page: status %d eof %v", status, eof)
			}
			seen := map[string]int{}
			for _, e := range page {
				seen[e.FileName]++
			}
			// the folder changes under the listing: one removed, one added
			must(t, g.tgt.Remove("/pg/e149"))
			delete(before, "e149")
			if _, err := g.tgt.Create("/pg/e999", 0o644); err != nil {
				t.Fatal(err)
			}
			for i := range others { // a busy client lists other folders meanwhile
				_, ofh, err := g.tgt.Lookup(fmt.Sprintf("/o%02d", i))
				must(t, err)
				readDirPage(t, g, ofh, 0, 0)
			}
			cookie := page[len(page)-1].Cookie
			for !eof {
				status, page, _, eof = readDirPage(t, g, fh, cookie, verf)
				if status != nfsc.NFS3Ok {
					t.Fatalf("next page after the folder changed: status %d, want OK", status)
				}
				for _, e := range page {
					seen[e.FileName]++
				}
				if len(page) > 0 {
					cookie = page[len(page)-1].Cookie
				}
			}
			for name := range before {
				if seen[name] != 1 {
					t.Errorf("%s listed %d times, want once", name, seen[name])
				}
			}
			if seen["e149"] != 0 {
				t.Error("e149, removed before its page, was listed")
			}
			// a new listing (cookie 0) carrying the old verifier lists the folder afresh
			fresh := map[string]bool{}
			cookie, eof = 0, false
			for !eof {
				var next uint64
				status, page, next, eof = readDirPage(t, g, fh, cookie, verf)
				verf = next // the client goes on with the verifier the listing answered
				if status != nfsc.NFS3Ok {
					t.Fatalf("a new listing: status %d", status)
				}
				for _, e := range page {
					fresh[e.FileName] = true
				}
				if len(page) > 0 {
					cookie = page[len(page)-1].Cookie
				}
			}
			if !fresh["e999"] || fresh["e149"] {
				t.Errorf("a new listing with the old verifier: e999 %v, e149 %v; want the folder as it is now", fresh["e999"], fresh["e149"])
			}
		})
	}
}
