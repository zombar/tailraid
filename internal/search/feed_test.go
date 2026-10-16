package search

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testseed"
)

// countingSrc counts the walks of every inode and the listings of each folder.
type countingSrc struct {
	Source
	mu    sync.Mutex
	walks int
	dirs  map[meta.Ino]int
}

func (c *countingSrc) ForEachInode(fn func(meta.Inode) bool) {
	c.mu.Lock()
	c.walks++
	c.mu.Unlock()
	c.Source.ForEachInode(fn)
}

func (c *countingSrc) ReadDir(ino meta.Ino) ([]meta.Inode, error) {
	c.mu.Lock()
	c.dirs[ino]++
	c.mu.Unlock()
	return c.Source.ReadDir(ino)
}

func (c *countingSrc) reset() (walks int, dirs map[meta.Ino]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	walks, dirs = c.walks, c.dirs
	c.walks, c.dirs = 0, map[meta.Ino]int{}
	return walks, dirs
}

func (e *env) counting() *countingSrc {
	c := &countingSrc{Source: e.f.FSM, dirs: map[meta.Ino]int{}}
	e.svc.Src = c
	return c
}

// create makes an empty file by a Create command (no flush): names tests need no data.
func (e *env) create(parent meta.Ino, name string) meta.Ino {
	e.t.Helper()
	return e.f.MustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: parent, Name: name, Type: meta.TFile, Mode: 0o644, Now: 1}}).Ino
}

// After its first build, a query brings the name list up to date from the change feed:
// a new name costs no walk of the inodes, however many there are (I-388).
// Invariant: I-388
func TestSearch_Names_UpdateCostIsTheChanges(t *testing.T) {
	e := newEnv(t)
	d := e.mkdir("/many")
	for i := range 2000 {
		e.create(d, fmt.Sprintf("file %d.bin", i))
	}
	c := e.counting()
	if len(e.search("file").Hits) == 0 {
		t.Fatal("no hits")
	}
	if walks, _ := c.reset(); walks != 1 {
		t.Fatalf("the first query walked %d times, want 1", walks)
	}
	e.create(d, "fresh one.bin")
	if got := paths(e.search("fresh")); !reflect.DeepEqual(got, []string{"name:/many/fresh one.bin"}) {
		t.Fatalf("hits %v", got)
	}
	if walks, dirs := c.reset(); walks != 0 || len(dirs) != 0 {
		t.Fatalf("a query after one create walked %d times and listed %v", walks, dirs)
	}
}

// The name list a query keeps up to date answers exactly what a list built from scratch
// does, over random creates, renames, moves, trashes, restores and deletes, inside
// /.tailraid and with ephemeral names among them (I-388).
// Invariant: I-388
func TestSearch_Names_FromFeed_MatchesRebuild(t *testing.T) {
	rng := testseed.Seed(t)
	e := newEnv(t)
	words := []string{"alpha", "beta", "gamma", "delta"}
	dirs := []meta.Ino{meta.RootIno, e.mkdir("/a"), e.mkdir("/a/b"), e.mkdir("/" + meta.ReservedDir + "/thumbs")}
	name := func() string {
		n := words[rng.Intn(len(words))] + " " + words[rng.Intn(len(words))] + fmt.Sprint(rng.Intn(5))
		if rng.Intn(10) == 0 {
			n = "." + n + ".tailraid-upload"
		}
		return n
	}
	for step := range 400 {
		d := dirs[rng.Intn(len(dirs))]
		kids, _ := e.f.FSM.ReadDir(d)
		switch op := rng.Intn(6); {
		case op <= 1 || len(kids) == 0:
			e.f.Apply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: d, Name: name(), Type: meta.TFile, Mode: 0o644, Now: 1}})
		case op == 2:
			k := kids[rng.Intn(len(kids))]
			e.f.Apply(meta.Command{Op: meta.OpRename, Rename: &meta.Rename{OldParent: d, OldName: k.Name, NewParent: dirs[rng.Intn(len(dirs))], NewName: name(), Now: 1}})
		case op == 3:
			k := kids[rng.Intn(len(kids))]
			e.f.Apply(meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: d, Name: k.Name, Recursive: true, Now: 1}})
		case op == 4:
			if roots := e.f.FSM.Trash(); len(roots) > 0 {
				e.f.Apply(meta.Command{Op: meta.OpRestore, Restore: &meta.Restore{Ino: roots[rng.Intn(len(roots))].Ino, Now: 1}})
			}
		default:
			if k := kids[rng.Intn(len(kids))]; k.Type == meta.TFile {
				e.f.Apply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: d, Name: k.Name, Now: 1}})
			}
		}
		if step%5 == 0 {
			q := words[rng.Intn(len(words))][:2+rng.Intn(3)]
			got, want := paths(e.svc.Search(context.Background(), q, "", MaxLimit)), paths(e.service().Search(context.Background(), q, "", MaxLimit))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("step %d %q: kept list %v, fresh list %v", step, q, got, want)
			}
		}
	}
}

// A pass after a change nothing indexes or catalogues (a folder made outside every
// library, a file written under /.tailraid) walks no inode, lists no library and
// writes no segment (I-387).
// Invariant: I-387
func TestPass_NothingChanged_WalksNothing(t *testing.T) {
	e := newEnv(t)
	e.library("/films", meta.LibraryMovies)
	e.write("/films/Heat (1995)/Heat (1995).mkv", []byte("video"))
	e.write("/notes.md", []byte("hello world"))
	c := e.counting()
	e.pass()
	segs := e.segments()
	if walks, _ := c.reset(); walks != 1 {
		t.Fatalf("the first pass walked %d times, want 1", walks)
	}
	e.mkdir("/elsewhere")
	e.write("/"+meta.ReservedDir+"/stats/x.bin", []byte{1, 2, 3})
	if res := e.pass(); res.Seq != 0 || res.Indexed != 0 {
		t.Fatalf("a pass with nothing to do wrote %+v", res)
	}
	walks, dirs := c.reset()
	if walks != 0 || dirs[e.ino("/films")] != 0 {
		t.Fatalf("a pass with nothing to do walked %d times and listed the library %d times", walks, dirs[e.ino("/films")])
	}
	if !reflect.DeepEqual(e.segments(), segs) {
		t.Fatal("a segment was written")
	}
}

// A change inside one library rebuilds that library's catalogue entries and lists
// nothing of the other; the result is the catalogue a full rebuild derives (I-387).
// Invariant: I-387
func TestPass_CatalogueRebuildsTouchedLibraryOnly(t *testing.T) {
	e := newEnv(t)
	e.library("/films", meta.LibraryMovies)
	e.library("/shows", meta.LibraryTV)
	e.write("/films/Heat (1995)/Heat (1995).mkv", []byte("video"))
	e.write("/shows/Lost/Season 1/Lost S01E01.mkv", []byte("video"))
	c := e.counting()
	e.pass()
	c.reset()
	e.write("/films/Alien (1979)/Alien (1979).mkv", []byte("video"))
	if res := e.pass(); res.Seq == 0 {
		t.Fatal("the new film wrote no segment")
	}
	_, dirs := c.reset()
	if dirs[e.ino("/shows")] != 0 || dirs[e.ino("/films")] == 0 {
		t.Fatalf("listings: shows %d, films %d", dirs[e.ino("/shows")], dirs[e.ino("/films")])
	}
	fresh := e.service()
	if res, err := fresh.Pass(context.Background()); err != nil || res.Seq != 0 {
		t.Fatalf("a full pass after it found more to write: %+v %v", res, err)
	}
}

// Over random writes, renames, trashes and restores in and out of libraries, the passes
// that follow the feed leave the index and the catalogue where a full walk finds nothing
// to add: a fresh service's first pass (always a walk) writes no segment (I-387).
// Invariant: I-387
func TestPass_FromFeed_MatchesFullWalk(t *testing.T) {
	rng := testseed.Seed(t)
	e := newEnv(t)
	e.library("/films", meta.LibraryMovies)
	e.library("/music", meta.LibraryMusic)
	places := []string{"/films/A (2001)", "/films/B (2002)", "/music/Band/Album", "/docs", "/docs/deep"}
	for _, p := range places {
		e.mkdir(p)
	}
	exts := []string{".mkv", ".md", ".txt", ".mp3", ".nfo"}
	e.pass()
	for round := range 12 {
		for range 4 {
			p := places[rng.Intn(len(places))]
			name := fmt.Sprintf("%s/f%d%s", p, rng.Intn(4), exts[rng.Intn(len(exts))])
			switch rng.Intn(4) {
			case 0, 1:
				e.write(name, fmt.Appendf(nil, "words %d round %d", rng.Intn(9), round))
			case 2:
				kids, _ := e.f.FSM.ReadDir(e.ino(p))
				if len(kids) > 0 {
					k := kids[rng.Intn(len(kids))]
					e.f.Apply(meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: e.ino(p), Name: k.Name, Recursive: true, Now: 1}})
				}
			default:
				if roots := e.f.FSM.Trash(); len(roots) > 0 {
					e.f.Apply(meta.Command{Op: meta.OpRestore, Restore: &meta.Restore{Ino: roots[rng.Intn(len(roots))].Ino, Now: 1}})
				}
			}
		}
		for res := e.pass(); res.Pending > 0; res = e.pass() {
		}
		if res, err := e.service().Pass(context.Background()); err != nil || res.Seq != 0 {
			t.Fatalf("round %d: a full pass found more to write: %+v %v", round, res, err)
		}
	}
}

// A track the pass indexes later than its write (the batch was full) still reaches its
// album: the pass that reads it derives that library again though the feed brought
// nothing new (I-387).
// Invariant: I-387
func TestPass_LateTrackRebuildsItsAlbum(t *testing.T) {
	e := newEnv(t)
	e.svc.Cfg.Batch = 1
	e.library("/music", meta.LibraryMusic)
	e.pass()
	for i := 1; i >= 0; i-- { // the album's first track last, so the full batch leaves it for the next pass
		e.write(fmt.Sprintf("/music/Agnes Obel/Aventine/%02d.mp3", i+1), id3(0, "TIT2", fmt.Sprintf("Track %d", i), "TPE1", "Agnes Obel", "TALB", "Aventine", "TYER", "2013"))
	}
	if res := e.pass(); res.Pending != 1 {
		t.Fatalf("first pass %+v, want one pending", res)
	}
	if res := e.pass(); res.Indexed != 1 || res.Pending != 0 {
		t.Fatalf("second pass %+v", res)
	}
	if res, err := e.service().Pass(context.Background()); err != nil || res.Seq != 0 {
		t.Fatalf("a full pass after it found more to write: %+v %v", res, err)
	}
}
