package pins_test

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
)

// listing counts each folder's listings.
type listing struct {
	pins.Store
	mu   sync.Mutex
	dirs map[meta.Ino]int
}

func (l *listing) ReadDir(ino meta.Ino) ([]meta.Inode, error) {
	l.mu.Lock()
	l.dirs[ino]++
	l.mu.Unlock()
	return l.Store.ReadDir(ino)
}

func (l *listing) reset() map[meta.Ino]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.dirs
	l.dirs = map[meta.Ino]int{}
	return d
}

// counted reopens the rig's pins over a store that counts listings.
func (r *rig) counted() *listing {
	r.t.Helper()
	l := &listing{Store: r.f.FSM, dirs: map[meta.Ino]int{}}
	p, err := pins.Open(r.dir, l, r.rd, r.cache, r.f.Clk, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	r.p = p
	return l
}

// After its first walk the worker lists nothing for a change outside every pin, and for
// a change under one pin lists that pin's folders only (I-389).
// Invariant: I-389
func TestPins_ChangeElsewhere_WalksNothing(t *testing.T) {
	r := newRig(t, 1<<30)
	music := r.mkdir(meta.RootIno, "music")
	films := r.mkdir(meta.RootIno, "films")
	other := r.mkdir(meta.RootIno, "other")
	r.file(music, "a", testharness.Pattern(100, 1))
	r.file(films, "b", testharness.Pattern(100, 2))
	l := r.counted()
	for _, p := range []string{"/music", "/films"} {
		if _, err := r.p.Pin(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	l.reset()
	r.file(other, "c", testharness.Pattern(100, 3))
	if walked, err := r.p.Follow(ctx); walked || err != nil {
		t.Fatalf("a change outside every pin: walked %v, %v", walked, err)
	}
	if d := l.reset(); len(d) != 0 {
		t.Fatalf("a change outside every pin listed %v", d)
	}
	if rep := r.p.List(); len(rep.Pins) != 2 {
		t.Fatalf("list %+v", rep)
	}
	if d := l.reset(); len(d) != 0 {
		t.Fatalf("List listed %v", d)
	}
	later := r.file(music, "d", testharness.Pattern(3*int(testharness.ChunkSize), 4))
	if walked, err := r.p.Follow(ctx); !walked || err != nil {
		t.Fatalf("a change under a pin: walked %v, %v", walked, err)
	}
	if d := l.reset(); d[films] != 0 || d[music] == 0 {
		t.Fatalf("a change under /music listed %v", d)
	}
	if !r.allCached(later) {
		t.Fatal("the new file was not fetched")
	}
}

// Over random writes, deletes and moves under two pins, the system folders and elsewhere,
// what the following worker reports and holds equals what a worker walking every pin
// from scratch finds (I-389).
// Invariant: I-389
func TestPins_FromFeed_MatchesFullWalk(t *testing.T) {
	rng := testseed.Seed(t)
	s := newSysRig(t)
	s.p.SetSystem(1 << 30)
	dirs := []meta.Ino{s.mkdir(meta.RootIno, "music"), s.mkdir(meta.RootIno, "films"), s.mkdir(meta.RootIno, "other")}
	dirs = append(dirs, s.mkdir(dirs[0], "album"))
	for _, p := range []string{"/music", "/films"} {
		if _, err := s.p.Pin(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	for step := range 40 {
		change(t, s, rng, dirs, step)
		if _, err := s.p.Follow(ctx); err != nil {
			t.Fatal(err)
		}
		fresh, err := pins.Open(s.dir, s.f.FSM, s.rd, s.cache, s.f.Clk, nil)
		if err != nil {
			t.Fatal(err)
		}
		fresh.SetSystem(1 << 30)
		if got, want := s.p.List(), fresh.List(); !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d: following %+v, from scratch %+v", step, got, want)
		}
	}
}

func change(t *testing.T, s *sysRig, rng *rand.Rand, dirs []meta.Ino, step int) {
	t.Helper()
	d := dirs[rng.Intn(len(dirs))]
	kids, _ := s.f.FSM.ReadDir(d)
	switch op := rng.Intn(5); {
	case op == 0:
		s.put([]string{"thumbs", "stats"}[rng.Intn(2)], fmt.Sprintf("s%d", step), 100+rng.Intn(400), byte(step))
	case op <= 2 || len(kids) == 0:
		s.file(d, fmt.Sprintf("f%d", step), testharness.Pattern(100+rng.Intn(3*int(testharness.ChunkSize)), byte(step)))
	case op == 3:
		k := kids[rng.Intn(len(kids))]
		s.f.Apply(meta.Command{Op: meta.OpRename, Rename: &meta.Rename{OldParent: d, OldName: k.Name, NewParent: dirs[rng.Intn(len(dirs))], NewName: filepath.Base(k.Name) + "m", Now: 1}})
	default:
		if k := kids[rng.Intn(len(kids))]; k.Type == meta.TFile {
			s.f.Apply(meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: d, Name: k.Name, Now: 1}})
		}
	}
}
