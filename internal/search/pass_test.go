package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// Two volumes given the same writes get byte-identical segments: the walk is in inode
// order and nothing in a segment depends on the node that wrote it.
// Invariant: I-207
func TestIndexer_SortedWalk_SegmentBytesDeterministic(t *testing.T) {
	read := func() []byte {
		e := newEnv(t)
		for i := range 12 {
			e.write(fmt.Sprintf("/d%d/note %d.txt", i%3, i), []byte(fmt.Sprintf("word%d shared lorem ipsum %d", i, i*i)))
		}
		e.write("/a/song.mp3", id3(0, "TIT2", "Riverside", "TPE1", "Agnès Obel"))
		if res := e.pass(); res.Indexed != 13 || res.Seq != 1 {
			t.Fatalf("pass %+v", res)
		}
		b, err := e.svc.Vol.ReadFile(context.Background(), Dir+"/1.seg")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if a, b := read(), read(); !bytes.Equal(a, b) {
		t.Fatalf("two volumes wrote different segments (%d and %d bytes)", len(a), len(b))
	}
}

// A file is re-read only when its chunk list changed: chmod, utimes and a rename cost
// nothing, a rewrite is indexed again.
// Invariant: I-207
func TestIndexer_ReindexOnlyWhenChunksChange(t *testing.T) {
	e := newEnv(t)
	e.write("/notes/a.txt", []byte("original words here"))
	if res := e.pass(); res.Indexed != 1 {
		t.Fatalf("first pass %+v", res)
	}
	if err := e.fs.Chmod("/notes/a.txt", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Chtimes("/notes/a.txt", time.Unix(5, 0), time.Unix(5, 0)); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename("/notes/a.txt", "/notes/b.txt"); err != nil {
		t.Fatal(err)
	}
	if res := e.pass(); res.Indexed != 0 || res.Seq != 0 {
		t.Fatalf("metadata changes re-indexed: %+v", res)
	}
	if got := paths(e.search("original")); !reflect.DeepEqual(got, []string{"content:/notes/b.txt"}) {
		t.Fatalf("after the rename: %v", got)
	}
	e.write("/notes/b.txt", []byte("replacement text"))
	if res := e.pass(); res.Indexed != 1 {
		t.Fatalf("rewrite not re-indexed: %+v", res)
	}
	if got := paths(e.search("original")); len(got) != 0 {
		t.Fatalf("old content still found: %v", got)
	}
	if got := paths(e.search("replacement")); !reflect.DeepEqual(got, []string{"content:/notes/b.txt"}) {
		t.Fatalf("new content: %v", got)
	}
}

// Nothing under /.tailraid, nothing in the trash, no ephemeral name and no other
// extension is read.
// Invariant: I-207
func TestIndexer_SkipsTrashReservedEphemeral(t *testing.T) {
	e := newEnv(t)
	e.write("/keep.txt", []byte("needle kept"))
	e.write("/.tailraid/x/secret.txt", []byte("needle reserved"))
	e.write("/._keep.txt", []byte("needle appledouble"))
	e.write("/photo.jpg", []byte("needle binary"))
	e.write("/gone.txt", []byte("needle deleted"))
	if err := e.fs.Remove("/gone.txt"); err != nil {
		t.Fatal(err)
	}
	if res := e.pass(); res.Indexed != 1 {
		t.Fatalf("pass %+v", res)
	}
	if got := paths(e.search("needle")); !reflect.DeepEqual(got, []string{"content:/keep.txt"}) {
		t.Fatalf("hits %v", got)
	}
}

// A pass with nothing changed writes nothing.
// Invariant: I-206
func TestIndexer_NoChangeWritesNothing(t *testing.T) {
	e := newEnv(t)
	if res := e.pass(); res.Seq != 0 || len(e.segments()) != 0 {
		t.Fatalf("empty volume wrote %+v %v", res, e.segments())
	}
	e.write("/a.txt", []byte("some words"))
	e.pass()
	before := e.segments()
	if res := e.pass(); res.Seq != 0 || !reflect.DeepEqual(e.segments(), before) {
		t.Fatalf("second pass wrote %+v %v", res, e.segments())
	}
}

// racingVolume loses every create: another leader made the name first.
type racingVolume struct{ Volume }

func (r racingVolume) Create(ctx context.Context, name string) error {
	if err := r.Volume.Create(ctx, name); err != nil {
		return err
	}
	return r.Volume.Create(ctx, name)
}

// A pass whose segment name another leader created first abandons, and the next pass
// on a fair volume writes past it.
// Invariant: I-206
func TestIndexer_LoserOfCreateRaceAborts(t *testing.T) {
	e := newEnv(t)
	e.write("/a.txt", []byte("racing words"))
	fair := e.svc.Vol
	e.svc.Vol = racingVolume{fair}
	if _, err := e.svc.Pass(context.Background()); !errors.Is(err, ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
	e.svc.Vol = fair
	if res := e.pass(); res.Seq != 2 || res.Indexed != 1 {
		t.Fatalf("next pass %+v", res)
	}
	if got := paths(e.search("racing")); len(got) != 1 {
		t.Fatalf("hits %v", got)
	}
}

// Only the first MaxFile bytes of a text file are indexed: a word inside them is found,
// one past them is not.
// Invariant: I-207
func TestIndexer_MaxFile_Table(t *testing.T) {
	const maxFile = 4096
	for _, tc := range []struct {
		end   int // where "zebra" ends
		found bool
	}{
		{5, true},
		{maxFile - 1, true},
		{maxFile, true},
		{maxFile + 1, false},
		{maxFile + 6, false},
	} {
		e := newEnv(t)
		e.svc.Cfg.MaxFile = maxFile
		b := bytes.Repeat([]byte(" "), tc.end+10)
		copy(b[tc.end-5:], "zebra")
		e.write("/big.txt", b)
		e.pass()
		if got := len(e.search("zebra").Hits) == 1; got != tc.found {
			t.Errorf("word ending at %d: found %v, want %v", tc.end, got, tc.found)
		}
	}
}

// An audio file's tags are read from its first chunk only: a tag that does not fit
// leaves the track indexed by name, never an error.
// Invariant: I-207
func TestIndexer_AudioFirstChunkOnly(t *testing.T) {
	e := newEnv(t)
	e.write("/music/a.mp3", id3(0, "TIT2", "Riverside", "TPE1", "Agnès Obel", "TALB", "Philharmonics", "TCON", "Pop"))
	e.write("/music/b.mp3", id3(int(e.f.FSM.Volume().ChunkSize)+100, "TIT2", "Faraway"))
	if res := e.pass(); res.Indexed != 2 || res.Pending != 0 {
		t.Fatalf("pass %+v", res)
	}
	r := e.search("agnes philharm")
	if len(r.Hits) != 1 || r.Hits[0].Match != "tags" || r.Hits[0].Title != "Riverside" || r.Hits[0].Artist != "Agnès Obel" || r.Hits[0].Album != "Philharmonics" {
		t.Fatalf("tag hit %+v", r.Hits)
	}
	if got := e.search("faraway"); len(got.Hits) != 0 {
		t.Fatalf("a tag past the first chunk was read: %+v", got.Hits)
	}
}

// Past RebuildAt the pass merges the chain into one segment: every live doc kept, a
// deleted one dropped.
// Invariant: I-206
func TestIndexer_RebuildWhenChainExceedsEight(t *testing.T) {
	e := newEnv(t)
	for i := range RebuildAt {
		e.write(fmt.Sprintf("/f%d.txt", i), []byte(fmt.Sprintf("unique%d common", i)))
		if res := e.pass(); res.Rebuilt || res.Seq != uint64(i+1) {
			t.Fatalf("pass %d: %+v", i, res)
		}
	}
	if err := e.fs.Remove("/f0.txt"); err != nil {
		t.Fatal(err)
	}
	e.write("/last.txt", []byte("unique99 common"))
	res := e.pass()
	if !res.Rebuilt {
		t.Fatalf("pass %+v", res)
	}
	x, err := Load(context.Background(), e.f.FSM, e.svc.Vol)
	if err != nil || len(x.Segs) != 1 || x.Segs[0].Seq != res.Seq || len(x.Segs[0].Chain) != 0 {
		t.Fatalf("after the rebuild: %v %+v", err, x)
	}
	if x.Docs() != RebuildAt {
		t.Fatalf("docs %d, want %d (one deleted, one added)", x.Docs(), RebuildAt)
	}
	if got := len(e.search("common").Hits); got != RebuildAt {
		t.Fatalf("common: %d hits", got)
	}
	if got := len(e.search("unique0").Hits); got != 0 {
		t.Fatal("the deleted file's words survived the rebuild")
	}
	if got := paths(e.search("unique99")); !reflect.DeepEqual(got, []string{"content:/last.txt"}) {
		t.Fatalf("unique99: %v", got)
	}
}

// Above MaxBytes the pass indexes tags only and says it is capped; a pass reads at most
// Batch candidates and BatchBytes of text.
// Invariant: I-207
func TestIndexer_MaxBytes_TagsOnly(t *testing.T) {
	e := newEnv(t)
	e.write("/first.txt", []byte("before the cap"))
	e.pass()
	e.svc.Cfg.MaxBytes = 1
	e.write("/second.txt", []byte("after the cap"))
	e.write("/song.mp3", id3(0, "TIT2", "Capped Song"))
	res := e.pass()
	if !res.Capped || res.Indexed != 1 {
		t.Fatalf("capped pass %+v", res)
	}
	if got := len(e.search("after").Hits); got != 0 {
		t.Fatal("text indexed past the cap")
	}
	if got := paths(e.search("capped")); !reflect.DeepEqual(got, []string{"tags:/song.mp3"}) {
		t.Fatalf("tags past the cap: %v", got)
	}
	if st := e.svc.Status(); !st.Capped {
		t.Fatalf("status %+v", st)
	}

	b := newEnv(t)
	b.svc.Cfg.Batch, b.svc.Cfg.BatchBytes = 3, 1<<20
	for i := range 5 {
		b.write(fmt.Sprintf("/n%d.txt", i), []byte("batch words"))
	}
	if res := b.pass(); res.Indexed != 3 || res.Pending != 2 {
		t.Fatalf("batch of 3: %+v", res)
	}
	if res := b.pass(); res.Indexed != 2 || res.Pending != 0 {
		t.Fatalf("the rest: %+v", res)
	}
	c := newEnv(t)
	c.svc.Cfg.BatchBytes = 10
	for i := range 3 {
		c.write(fmt.Sprintf("/m%d.txt", i), []byte("eight by")) // 8 bytes each
	}
	if res := c.pass(); res.Indexed != 1 || res.Pending != 2 {
		t.Fatalf("batch bytes 10: %+v", res)
	}
}

// The sweep removes what lies outside the chain once it is an hour old, never the chain.
// Invariant: I-206
func TestSweep_RemovesOrphansOlderThanHour(t *testing.T) {
	e := newEnv(t)
	for i := range RebuildAt + 1 {
		e.write(fmt.Sprintf("/f%d.txt", i), []byte(fmt.Sprintf("word%d", i)))
		e.pass()
	}
	if got := len(e.segments()); got != RebuildAt+1 {
		t.Fatalf("segments %d", got)
	}
	ctx := context.Background()
	if n, err := e.svc.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("young sweep: %d %v", n, err)
	}
	e.f.Clk.Advance(SweepAge + time.Minute)
	if n, err := e.svc.Sweep(ctx); err != nil || n != RebuildAt {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if got := e.segments(); !reflect.DeepEqual(got, []string{fmt.Sprintf("%d.seg", RebuildAt+1)}) {
		t.Fatalf("left %v", got)
	}
	if got := len(e.search("word3").Hits); got != 1 {
		t.Fatal("the sweep broke the index")
	}
}

// A node's index reloads when another node's pass wrote a segment.
// Invariant: I-206
func TestSearch_ReloadsOnNewSegment(t *testing.T) {
	e := newEnv(t)
	reader := e.service()
	e.write("/a.txt", []byte("first"))
	e.pass()
	if len(reader.Search(context.Background(), "first", "", 0).Hits) != 1 {
		t.Fatal("first segment not read")
	}
	e.write("/b.txt", []byte("second"))
	e.pass()
	if got := paths(reader.Search(context.Background(), "second", "", 0)); !reflect.DeepEqual(got, []string{"content:/b.txt"}) {
		t.Fatalf("not reloaded: %v", got)
	}
}

// Words are ANDed with the last a prefix; names come first, then tags, then content.
// Invariant: I-208
func TestSearch_AndPrefix_RankNameTagsContent(t *testing.T) {
	e := newEnv(t)
	e.write("/notes/alpha beta.txt", []byte("gamma delta"))
	e.write("/music/song.mp3", id3(0, "TIT2", "Alpha Song"))
	e.write("/docs/readme.md", []byte("intro alpha alpha alpha betamax"))
	e.write("/docs/other.md", []byte("alpha once"))
	e.pass()
	if got := paths(e.search("Alpha")); !reflect.DeepEqual(got, []string{"name:/notes/alpha beta.txt", "tags:/music/song.mp3", "content:/docs/readme.md", "content:/docs/other.md"}) {
		t.Fatalf("alpha: %v", got)
	}
	r := e.search("alpha bet")
	if got := paths(r); !reflect.DeepEqual(got, []string{"name:/notes/alpha beta.txt", "content:/docs/readme.md"}) {
		t.Fatalf("alpha bet: %v", got)
	}
	if terms := r.Hits[1].Terms; !reflect.DeepEqual(terms, []TermHit{{"alpha", 6}, {"betamax", 24}}) {
		t.Fatalf("terms %v", terms)
	}
	if got := paths(e.search("bet alpha")); len(got) != 0 {
		t.Fatalf("only the last word is a prefix: %v", got)
	}
	if got := paths(e.search("  ,, ")); len(got) != 0 {
		t.Fatalf("an empty query: %v", got)
	}
}

// Hits outside under, in the trash or under /.tailraid never appear.
// Invariant: I-208
func TestSearch_FiltersDeletedAndUnder(t *testing.T) {
	e := newEnv(t)
	e.write("/docs/readme.md", []byte("needle"))
	e.write("/other/needle.txt", []byte("hay"))
	e.write("/.tailraid/needle.txt", []byte("needle"))
	e.pass()
	ctx := context.Background()
	if got := paths(e.svc.Search(ctx, "needle", "/docs", 0)); !reflect.DeepEqual(got, []string{"content:/docs/readme.md"}) {
		t.Fatalf("under /docs: %v", got)
	}
	if got := paths(e.svc.Search(ctx, "needle", "/nowhere", 0)); len(got) != 0 {
		t.Fatalf("under a missing folder: %v", got)
	}
	if err := e.fs.Remove("/docs/readme.md"); err != nil {
		t.Fatal(err)
	}
	e.f.Clk.Advance(NameRefresh)
	if got := paths(e.search("needle")); !reflect.DeepEqual(got, []string{"name:/other/needle.txt"}) {
		t.Fatalf("after the delete: %v", got)
	}
}

// limit: 0 is the default, more than the maximum is the maximum.
// Invariant: I-208
func TestSearch_LimitBoundaries(t *testing.T) {
	e := newEnv(t)
	for i := range MaxLimit + 5 {
		e.mkdir(fmt.Sprintf("/many/item %03d", i))
	}
	for _, tc := range []struct{ limit, want int }{{0, DefaultLimit}, {1, 1}, {MaxLimit - 1, MaxLimit - 1}, {MaxLimit, MaxLimit}, {MaxLimit + 1, MaxLimit}} {
		r := e.svc.Search(context.Background(), "item", "", tc.limit)
		if len(r.Hits) != tc.want {
			t.Errorf("limit %d: %d hits, want %d", tc.limit, len(r.Hits), tc.want)
		}
		if len(r.Hits) > 0 && r.Hits[0].Path != "/many/item 000" {
			t.Errorf("limit %d: first %s", tc.limit, r.Hits[0].Path)
		}
	}
}

// The name list follows the FSM: a new name is found once NameRefresh has passed, with
// no pass at all.
// Invariant: I-208
func TestSearch_NameListFollowsApplied(t *testing.T) {
	e := newEnv(t)
	if len(e.search("fresh").Hits) != 0 {
		t.Fatal("found before written")
	}
	e.write("/fresh file.bin", []byte{1})
	e.f.Clk.Advance(NameRefresh)
	if got := paths(e.search("fresh")); !reflect.DeepEqual(got, []string{"name:/fresh file.bin"}) {
		t.Fatalf("hits %v", got)
	}
	if err := e.fs.Rename("/fresh file.bin", "/renamed.bin"); err != nil {
		t.Fatal(err)
	}
	e.f.Clk.Advance(NameRefresh)
	if got := paths(e.search("renamed")); !reflect.DeepEqual(got, []string{"name:/renamed.bin"}) {
		t.Fatalf("after the rename %v", got)
	}
	if strings.Contains(fmt.Sprint(paths(e.search("fresh"))), "fresh") {
		t.Fatal("the old name is still found")
	}
	_ = meta.RootIno
}
