package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
	"github.com/zombar/tailraid/internal/testseed"
)

const testCS = 16 // a tiny chunk size keeps every case small

// fakeVolume is a Source: directories by ino, chunks by hash, some unreadable.
type fakeVolume struct {
	dirs       map[uint64][]Entry
	chunks     map[meta.Hash][]byte
	unreadable map[meta.Hash]bool
	reads      atomic.Int64     // Chunk runs on the writer's workers
	onDir      func(ino uint64) // a hook a test runs while the walk is under way
}

func newVolume() *fakeVolume {
	return &fakeVolume{dirs: map[uint64][]Entry{1: nil}, chunks: map[meta.Hash][]byte{}, unreadable: map[meta.Hash]bool{}}
}

func (v *fakeVolume) Dir(ino uint64) ([]Entry, error) {
	if v.onDir != nil {
		v.onDir(ino)
	}
	es, ok := v.dirs[ino]
	if !ok {
		return nil, fmt.Errorf("no directory %d", ino)
	}
	return slices.Clone(es), nil
}

func (v *fakeVolume) Chunk(_ context.Context, h meta.Hash) ([]byte, error) {
	v.reads.Add(1)
	if v.unreadable[h] {
		return nil, errors.New("fewer than k live slots")
	}
	b, ok := v.chunks[h]
	if !ok {
		return nil, fmt.Errorf("no chunk %x", h[:4])
	}
	return b, nil
}

// put adds a file under dir with the given content, split into testCS chunks; an
// all-zero chunk is a hole, as the flusher writes it.
func (v *fakeVolume) put(dir uint64, name string, ino uint64, content []byte) {
	e := Entry{Name: name, Type: TypeFile, Mode: 0o640, Mtime: int64(ino) * 1e9, Ino: ino, Gen: 1, Size: uint64(len(content))}
	for off := 0; off < len(content); off += testCS {
		c := content[off:min(off+testCS, len(content))]
		if bytes.Count(c, []byte{0}) == len(c) {
			e.Chunks = append(e.Chunks, meta.HashZero)
			continue
		}
		h := meta.Hash(sha256.Sum256(c))
		v.chunks[h] = slices.Clone(c)
		e.Chunks = append(e.Chunks, h)
	}
	v.replace(dir, e)
}

func (v *fakeVolume) mkdir(dir uint64, name string, ino uint64) {
	v.dirs[ino] = nil
	v.replace(dir, Entry{Name: name, Type: TypeDir, Mode: 0o750, Mtime: 5e9, Ino: ino, Gen: 1})
}

func (v *fakeVolume) replace(dir uint64, e Entry) {
	es := v.dirs[dir]
	for i := range es {
		if es[i].Name == e.Name {
			es[i] = e
			return
		}
	}
	v.dirs[dir] = append(es, e)
}

func newMirror(t *testing.T, fsys sysfs.FS) (*Mirror, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "m")
	if err := Create(fsys, root, Marker{ClusterID: "aa", MirrorID: "m1", HostID: "h1", Created: 1}); err != nil {
		t.Fatal(err)
	}
	m, err := Open(fsys, root, "aa")
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

// complete checks I-180's promise: every tree and object a manifest names (but its
// listed missing chunks) is on the disk and verifies.
func complete(t *testing.T, m *Mirror) {
	t.Helper()
	mans, err := m.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	for i := range mans {
		man := &mans[i]
		missing := map[meta.Hash]bool{}
		for _, h := range man.Missing {
			missing[meta.Hash(h)] = true
		}
		var walk func(h meta.Hash)
		walk = func(h meta.Hash) {
			tr, err := m.Tree(h)
			if err != nil {
				t.Fatalf("snapshot %d: tree %x: %v", man.ID, h[:4], err)
			}
			for i := range tr.Entries {
				e := &tr.Entries[i]
				switch e.Type {
				case TypeDir:
					walk(e.Child)
				case TypeFile:
					for _, c := range e.Chunks {
						if c == meta.HashZero || missing[c] {
							continue
						}
						if _, err := m.Object(c); err != nil {
							t.Fatalf("snapshot %d: %s: %v", man.ID, e.Name, err)
						}
					}
				case TypeSymlink:
				}
			}
		}
		walk(meta.Hash(man.Root))
	}
}

func snap(t *testing.T, m *Mirror, v *fakeVolume, id uint32) Manifest {
	t.Helper()
	man, err := m.WriteSnapshot(context.Background(), v, 1, Manifest{ID: id, Name: fmt.Sprintf("s%d", id), Index: uint64(id) * 10, ChunkSize: testCS})
	if err != nil {
		t.Fatal(err)
	}
	return man
}

// Invariant: I-180
func TestMirror_TreeEncoding_Canonical(t *testing.T) {
	a := Entry{Name: "a", Type: TypeDir}
	b := Entry{Name: "b", Type: TypeDir}
	ab, err := EncodeTree(Tree{Entries: []Entry{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	ba, _ := EncodeTree(Tree{Entries: []Entry{b, a}})
	if !bytes.Equal(ab, ba) || TreeHash(ab) != TreeHash(ba) {
		t.Fatal("entry order changed the encoding")
	}
	got, err := DecodeTree(ab)
	if err != nil || len(got.Entries) != 2 || got.Entries[0].Name != "a" {
		t.Fatalf("decode %+v %v", got, err)
	}
	if e, ok := got.Lookup("b"); !ok || e.Name != "b" {
		t.Fatal("lookup")
	}
	if _, ok := got.Lookup("c"); ok {
		t.Fatal("lookup of an absent name")
	}
	c := a
	c.Mtime = 1
	if ac, _ := EncodeTree(Tree{Entries: []Entry{c, b}}); TreeHash(ac) == TreeHash(ab) {
		t.Fatal("an attribute change kept the hash")
	}
	for name, es := range map[string][]Entry{
		"duplicate":    {a, a},
		"slash":        {{Name: "x/y", Type: TypeDir}},
		"dot":          {{Name: "..", Type: TypeDir}},
		"empty name":   {{Name: "", Type: TypeDir}},
		"unknown type": {{Name: "x", Type: 9}},
		"long target":  {{Name: "x", Type: TypeSymlink, Target: string(make([]byte, maxTargetLen+1))}},
	} {
		if _, err := EncodeTree(Tree{Entries: es}); !errors.Is(err, ErrBadTree) {
			t.Errorf("%s: %v, want ErrBadTree", name, err)
		}
	}
	swapped := slices.Clone(ab)
	i, j := bytes.IndexByte(swapped, 'a'), bytes.IndexByte(swapped, 'b')
	swapped[i], swapped[j] = 'b', 'a'
	typed := slices.Clone(ab)
	typed[i+1] = 9
	for name, in := range map[string][]byte{
		"magic":     append([]byte("XRT1"), ab[4:]...),
		"order":     swapped,
		"type":      typed,
		"trailing":  append(slices.Clone(ab), 0),
		"truncated": ab[:len(ab)-1],
	} {
		if _, err := DecodeTree(in); !errors.Is(err, ErrBadTree) {
			t.Errorf("%s: %v, want ErrBadTree", name, err)
		}
	}
}

// Invariant: I-180
// Lifecycle: L-214
func TestMirror_SecondSyncWritesOnlyChanges(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.mkdir(1, "keep", 2)
	v.mkdir(1, "edit", 3)
	for i := range 8 {
		v.put(2, fmt.Sprintf("k%d", i), uint64(10+i), bytes.Repeat([]byte{byte(i + 1)}, 3*testCS))
	}
	v.put(3, "doc", 20, append(bytes.Repeat([]byte{'x'}, testCS), bytes.Repeat([]byte{'y'}, testCS)...))
	first := snap(t, m, v, 1)
	if first.NewBytes == 0 || int64(first.NewBytes) != m.Used() {
		t.Fatalf("first sync added %d, the mirror holds %d", first.NewBytes, m.Used())
	}
	if first.Files != 9 || first.Dirs != 3 || first.Bytes != 8*3*testCS+2*testCS {
		t.Fatalf("first manifest %+v", first)
	}
	reads := v.reads.Load()
	// change one chunk of one file: the second sync writes that object and the two trees
	// above it (edit/ and the root), nothing else
	v.put(3, "doc", 20, append(bytes.Repeat([]byte{'x'}, testCS), bytes.Repeat([]byte{'z'}, testCS)...))
	before := m.Used()
	second := snap(t, m, v, 2)
	if v.reads.Load()-reads != 1 {
		t.Fatalf("second sync read %d chunks from the pool, want 1", v.reads.Load()-reads)
	}
	if int64(second.NewBytes) != m.Used()-before {
		t.Fatalf("NewBytes %d, the mirror grew %d", second.NewBytes, m.Used()-before)
	}
	trees, _ := treeCount(t, m)
	if trees != 3+2 {
		t.Fatalf("%d trees on the mirror, want 5: the unchanged keep/ is stored once", trees)
	}
	if second.NewBytes >= first.NewBytes/4 {
		t.Fatalf("second sync added %d of the first's %d: not differential", second.NewBytes, first.NewBytes)
	}
	// a third sync of an unchanged volume adds nothing but its manifest
	if third := snap(t, m, v, 3); third.NewBytes != 0 || third.Root != second.Root {
		t.Fatalf("an unchanged volume added %d bytes", third.NewBytes)
	}
	complete(t, m)
}

func treeCount(t *testing.T, m *Mirror) (trees, objects int) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.trees), len(m.objects)
}

// Invariant: I-180
func TestMirror_CrashAtEveryWrite_ManifestImpliesComplete(t *testing.T) {
	v := newVolume()
	v.mkdir(1, "d", 2)
	v.put(2, "a", 10, bytes.Repeat([]byte{1}, 2*testCS))
	v.put(1, "b", 11, bytes.Repeat([]byte{2}, testCS+3))
	boom := errors.New("injected")
	for _, step := range []sysfstest.Step{sysfstest.StepCreateTemp, sysfstest.StepWrite, sysfstest.StepSync, sysfstest.StepClose, sysfstest.StepRename, sysfstest.StepSyncDir} {
		for n := 1; ; n++ {
			fault := sysfstest.New(sysfs.OS{})
			m, root := newMirror(t, fault)
			fault.FailAtNth(step, n, boom)
			_, err := m.WriteSnapshot(context.Background(), v, 1, Manifest{ID: 1, Name: "s", ChunkSize: testCS})
			// whatever failed, a reopened mirror has no temp, and a manifest on it names a
			// complete snapshot
			re, oerr := Open(sysfs.OS{}, root, "aa")
			if oerr != nil {
				t.Fatal(oerr)
			}
			if ents, _ := os.ReadDir(filepath.Join(root, "tmp")); len(ents) != 0 {
				t.Fatalf("%s #%d: %d temps left after open", step, n, len(ents))
			}
			complete(t, re)
			mans, _ := re.Manifests()
			if err == nil {
				if len(mans) != 1 {
					t.Fatalf("%s #%d: a successful write left %d manifests", step, n, len(mans))
				}
				break // every call of this step has had its turn
			}
			if !errors.Is(err, boom) {
				t.Fatalf("%s #%d: %v", step, n, err)
			}
			// the next sync trusts what the failed one left: it must still end complete
			if _, err := re.WriteSnapshot(context.Background(), v, 1, Manifest{ID: 2, Name: "t", ChunkSize: testCS}); err != nil {
				t.Fatalf("%s #%d: retry: %v", step, n, err)
			}
			complete(t, re)
		}
	}
}

// Invariant: I-180
func TestMirror_ObjectVerifiedOnRead(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.put(1, "f", 10, bytes.Repeat([]byte{7}, testCS))
	man := snap(t, m, v, 1)
	h := v.dirs[1][0].Chunks[0]
	if !m.HasObject(h) || !m.HasTree(meta.Hash(man.Root)) {
		t.Fatal("not indexed")
	}
	if _, err := m.PutObject(h, []byte("not these bytes")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a put of bytes that are not the chunk: %v", err)
	}
	p := m.path("objects", h)
	raw, _ := os.ReadFile(p)
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Object(h); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a flipped object byte: %v", err)
	}
	// an intact object under another name
	other := meta.Hash{9}
	if err := os.MkdirAll(filepath.Dir(m.path("objects", other)), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutObject(meta.Hash(sha256.Sum256([]byte("abc"))), []byte("abc")); err != nil {
		t.Fatal(err)
	}

	src, _ := os.ReadFile(m.path("objects", meta.Hash(sha256.Sum256([]byte("abc")))))
	if err := os.WriteFile(m.path("objects", other), src, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Object(other); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("an object under a wrong name: %v", err)
	}
	tp := m.path("trees", meta.Hash(man.Root))
	raw, _ = os.ReadFile(tp)
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(tp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Tree(meta.Hash(man.Root)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a flipped tree byte: %v", err)
	}
	if _, err := m.Object(meta.Hash{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an absent object: %v", err)
	}
}

// A mirror unplugged while it is open (its directory gone, as an unmounted disk leaves
// an empty mount point or none) refuses every write rather than recreate its path on
// the host's own disk: an object, a tree and a manifest all fail and the path stays
// absent; plugged back, the same handle writes again.
// Invariant: I-180
func TestMirror_DetachedRootNotRecreated(t *testing.T) {
	m, root := newMirror(t, sysfs.OS{})
	away := root + ".away"
	if err := os.Rename(root, away); err != nil {
		t.Fatal(err)
	}
	data := []byte("written while unplugged")
	h := meta.Hash(sha256.Sum256(data))
	if _, err := m.PutObject(h, data); err == nil {
		t.Error("an object written to an unplugged mirror")
	}
	v := newVolume()
	v.put(1, "f", 10, bytes.Repeat([]byte{3}, testCS))
	if _, err := m.WriteSnapshot(context.Background(), v, 1, Manifest{ID: 1, Name: "s", ChunkSize: testCS}); err == nil {
		t.Error("a snapshot written to an unplugged mirror")
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the unplugged mirror's path was recreated: %v", err)
	}
	if err := os.Rename(away, root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutObject(h, data); err != nil {
		t.Fatalf("plugged back: %v", err)
	}
}

// Invariant: I-180
func TestMirror_OpenSweepsTemps(t *testing.T) {
	m, root := newMirror(t, sysfs.OS{})
	tmp := filepath.Join(root, "tmp", "w-123")
	if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(sysfs.OS{}, root, ""); err != nil { // a standalone reader writes nothing
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("a read-only open removed a temp")
	}
	if _, err := Open(sysfs.OS{}, root, "AA"); err != nil { // cluster IDs compare as hex, any case
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("open left a temp")
	}
	if mk := m.Marker(); mk.MirrorID != "m1" || mk.Format != Format || m.Root() != root {
		t.Fatalf("marker %+v", mk)
	}
}

// Invariant: I-180
func TestMirror_ExportAndCat_NoCluster(t *testing.T) {
	m, root := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.mkdir(1, "music", 2)
	content := append(append(bytes.Repeat([]byte{'a'}, testCS), make([]byte, testCS)...), []byte("tail")...) // a hole and a short last chunk
	v.put(2, "song", 10, content)
	v.put(1, "lost", 11, bytes.Repeat([]byte{'q'}, testCS))
	v.unreadable[v.dirs[1][1].Chunks[0]] = true
	v.replace(1, Entry{Name: "link", Type: TypeSymlink, Mode: 0o777, Ino: 12, Gen: 1, Target: "music/song", Mtime: 1e9})
	man := snap(t, m, v, 1)
	if len(man.Missing) != 1 {
		t.Fatalf("missing %v, want the unreadable chunk", man.Missing)
	}
	// no cluster: open by path alone, list, read, export
	ro, err := Open(sysfs.OS{}, root, "")
	if err != nil {
		t.Fatal(err)
	}
	mans, err := ro.Manifests()
	if err != nil || len(mans) != 1 || mans[0].Root != man.Root || mans[0].Name != "s1" {
		t.Fatalf("manifests %+v %v", mans, err)
	}
	e, err := ro.Resolve(meta.Hash(man.Root), "/music/song")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ro.ReadFile(e, man.ChunkSize, &buf); err != nil || !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("cat: %v, %d bytes", err, buf.Len())
	}
	dst := filepath.Join(t.TempDir(), "out")
	err = ro.Export(mans[0], "/", dst)
	if err == nil {
		t.Fatal("an export with a missing chunk reported nothing")
	}
	got, rerr := os.ReadFile(filepath.Join(dst, "music", "song"))
	if rerr != nil || !bytes.Equal(got, content) {
		t.Fatalf("exported song: %v", rerr)
	}
	fi, _ := os.Stat(filepath.Join(dst, "music", "song"))
	if !fi.ModTime().Equal(time.Unix(10, 0)) {
		t.Fatalf("mtime %v", fi.ModTime())
	}
	if runtime.GOOS != "windows" { // Windows keeps no Unix modes, and makes symlinks only with a privilege
		if fi.Mode().Perm() != 0o640 {
			t.Fatalf("mode %v", fi.Mode())
		}
		if target, _ := os.Readlink(filepath.Join(dst, "link")); target != "music/song" {
			t.Fatalf("link target %q", target)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "lost")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a file with a missing chunk was exported partly")
	}
	sub := filepath.Join(t.TempDir(), "one")
	if err := ro.Export(mans[0], "/music", sub); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(sub, "music", "song")); !bytes.Equal(got, content) {
		t.Fatal("export of a sub-tree")
	}
	if _, err := ro.Resolve(meta.Hash(man.Root), "/music/song/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a path through a file: %v", err)
	}
}

// Invariant: I-183, I-243
func TestMirror_DeleteReconciles_MarkAndSweep(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.put(1, "shared", 10, bytes.Repeat([]byte{1}, testCS))
	v.put(1, "old", 11, bytes.Repeat([]byte{2}, testCS))
	snap(t, m, v, 1)
	v.dirs[1] = v.dirs[1][:1] // "old" deleted before the second snapshot
	v.put(1, "new", 12, bytes.Repeat([]byte{3}, testCS))
	snap(t, m, v, 2)
	_, objects := treeCount(t, m)
	if objects != 3 {
		t.Fatalf("%d objects, want 3", objects)
	}
	// thinning marked snapshot 1 Deleting: its manifest goes, then what only it reached
	removed, imported, freed, err := m.Reconcile([]Row{{ID: 1, Deleting: true}, {ID: 2}})
	if err != nil || !slices.Equal(removed, []uint32{1}) || len(imported) != 0 || freed == 0 {
		t.Fatalf("reconcile %v %v %d %v", removed, imported, freed, err)
	}
	if m.HasObject(meta.Hash(sha256.Sum256(bytes.Repeat([]byte{2}, testCS)))) {
		t.Fatal("the object only snapshot 1 reached survived")
	}
	if !m.HasObject(meta.Hash(sha256.Sum256(bytes.Repeat([]byte{1}, testCS)))) {
		t.Fatal("a shared object was swept")
	}
	complete(t, m)
	// a crash after the sweep and before the proposal: the next reconcile still reports it
	if removed, _, _, _ := m.Reconcile([]Row{{ID: 1, Deleting: true}, {ID: 2}}); !slices.Equal(removed, []uint32{1}) {
		t.Fatalf("second reconcile %v", removed)
	}
}

// Invariant: I-183
func TestMirror_SweepNeverTakesReachable(t *testing.T) {
	rng := testseed.Seed(t)
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	next := uint64(10)
	dirs := []uint64{1}
	var rows []Row
	for id := uint32(1); id <= 30; id++ {
		for range 1 + rng.Intn(4) {
			switch rng.Intn(4) {
			case 0:
				next++
				v.mkdir(dirs[rng.Intn(len(dirs))], fmt.Sprintf("d%d", next), next)
				dirs = append(dirs, next)
			default:
				next++
				b := make([]byte, rng.Intn(3*testCS))
				for i := range b {
					b[i] = byte(rng.Intn(4)) // few distinct chunks: plenty of sharing
				}
				v.put(dirs[rng.Intn(len(dirs))], fmt.Sprintf("f%d", rng.Intn(6)), next, b)
			}
		}
		snap(t, m, v, id)
		rows = append(rows, Row{ID: id})
		for i := range rows {
			if rng.Intn(5) == 0 {
				rows[i].Deleting = true
			}
		}
		if _, _, _, err := m.Reconcile(rows); err != nil {
			t.Fatal(err)
		}
		live := rows[:0]
		for _, r := range rows {
			if !r.Deleting {
				live = append(live, r)
			}
		}
		rows = live
		complete(t, m)
	}
	// and nothing unreachable is left: a sweep now frees nothing
	if n, _, err := m.Sweep(); err != nil || n != 0 {
		t.Fatalf("a second sweep removed %d (%v)", n, err)
	}
}

// Invariant: I-183
func TestMirror_SweepKeepsActiveWrite(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.mkdir(1, "a", 2)
	v.mkdir(1, "b", 3)
	v.put(2, "x", 10, bytes.Repeat([]byte{4}, testCS))
	v.put(3, "y", 11, bytes.Repeat([]byte{5}, testCS))
	v.onDir = func(ino uint64) {
		if ino == 3 { // a/ is written, its manifest is not: a sweep now must keep it
			if _, _, err := m.Sweep(); err != nil {
				t.Error(err)
			}
		}
	}
	snap(t, m, v, 1)
	complete(t, m)
}

// Invariant: I-183
func TestMirror_RowlessManifestImported(t *testing.T) {
	mans := []Manifest{{ID: 1}, {ID: 2}, {ID: 3}}
	remove, imported := Plan([]Row{{ID: 1, Deleting: true}, {ID: 2}, {ID: 9, Deleting: true}}, mans)
	if !slices.Equal(remove, []uint32{1}) || len(imported) != 1 || imported[0].ID != 3 {
		t.Fatalf("plan: remove %v, import %+v", remove, imported)
	}
	// on a disk: a mirror removed and added back has no rows; every manifest is imported
	// and none is removed
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.put(1, "f", 10, bytes.Repeat([]byte{6}, testCS))
	snap(t, m, v, 1)
	snap(t, m, v, 2)
	removed, imp, _, err := m.Reconcile(nil)
	if err != nil || len(removed) != 0 || len(imp) != 2 {
		t.Fatalf("reconcile without rows: %v %d %v", removed, len(imp), err)
	}
	if got, _ := m.Manifests(); len(got) != 2 {
		t.Fatalf("%d manifests left, want 2", len(got))
	}
	complete(t, m)
}

// Invariant: I-183
func TestMirror_ForeignClusterUntouched(t *testing.T) {
	_, root := newMirror(t, sysfs.OS{})
	tmp := filepath.Join(root, "tmp", "w-1")
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(sysfs.OS{}, root, "bb"); !errors.Is(err, ErrForeign) {
		t.Fatalf("open for another cluster: %v", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("a foreign mirror was changed")
	}
	if err := Create(sysfs.OS{}, root, Marker{ClusterID: "bb", MirrorID: "m2"}); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("create over a mirror: %v", err)
	}
	if mk, err := ReadMarker(sysfs.OS{}, root); err != nil || mk.ClusterID != "aa" {
		t.Fatalf("marker after the refusals: %+v %v", mk, err)
	}
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "data"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Create(sysfs.OS{}, full, Marker{ClusterID: "aa", MirrorID: "m3"}); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("create over data: %v", err)
	}
	if _, err := ReadMarker(sysfs.OS{}, full); !errors.Is(err, ErrNotMirror) {
		t.Fatalf("a directory without a marker: %v", err)
	}
	if _, err := Open(sysfs.OS{}, full, "aa"); !errors.Is(err, ErrNotMirror) {
		t.Fatalf("open of a directory without a marker: %v", err)
	}
}

// A sync never replaces a manifest on the disk, and an imported manifest moves to its
// row's ID: written first, the old file removed after, and a crash between the two
// leaves a copy that the next renumber removes (docs/16 §5).
// Invariant: I-183
func TestMirror_RenumberNeverReplaces(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.put(1, "f", 10, bytes.Repeat([]byte{7}, testCS))
	first := snap(t, m, v, 3)
	if _, err := m.WriteSnapshot(context.Background(), v, 1, Manifest{ID: 3, Name: "other", ChunkSize: testCS}); !errors.Is(err, ErrExists) {
		t.Fatalf("a sync over manifest 3: %v", err)
	}
	if mans, _ := m.Manifests(); len(mans) != 1 || mans[0].Name != first.Name {
		t.Fatalf("manifests after the refused sync %+v", mans)
	}
	if err := m.Renumber(3, 9, first.Name); err != nil {
		t.Fatal(err)
	}
	mans, _ := m.Manifests()
	if len(mans) != 1 || mans[0].ID != 9 || mans[0].Name != first.Name || mans[0].Root != first.Root {
		t.Fatalf("after renumber %+v", mans)
	}
	// the crash between the write and the remove: both files, the same snapshot
	dup := mans[0]
	dup.ID = 4
	if err := m.PutManifest(dup); err != nil {
		t.Fatal(err)
	}
	if err := m.Renumber(4, 9, first.Name); err != nil {
		t.Fatal(err)
	}
	if mans, _ := m.Manifests(); len(mans) != 1 || mans[0].ID != 9 {
		t.Fatalf("the copy survived: %+v", mans)
	}
	// another snapshot under the target ID: refused, nothing changes
	snap(t, m, v, 5)
	if err := m.Renumber(5, 9, "s5"); !errors.Is(err, ErrExists) {
		t.Fatalf("renumber onto another snapshot: %v", err)
	}
	if mans, _ := m.Manifests(); len(mans) != 2 {
		t.Fatalf("manifests after the refused renumber %+v", mans)
	}
	complete(t, m)
}

// Invariant: I-188
func TestMirror_AdoptKeepsIDRewritesCluster(t *testing.T) {
	m, root := newMirror(t, sysfs.OS{})
	v := newVolume()
	v.put(1, "f", 10, bytes.Repeat([]byte{8}, testCS))
	snap(t, m, v, 1)
	mk, err := Adopt(sysfs.OS{}, root, "bb", "h2")
	if err != nil || mk.ClusterID != "bb" || mk.HostID != "h2" || mk.MirrorID != "m1" {
		t.Fatalf("adopt %+v %v", mk, err)
	}
	if _, err := Open(sysfs.OS{}, root, "aa"); !errors.Is(err, ErrForeign) {
		t.Fatalf("the old cluster still opens it: %v", err)
	}
	m2, err := Open(sysfs.OS{}, root, "bb")
	if err != nil {
		t.Fatal(err)
	}
	if mans, _ := m2.Manifests(); len(mans) != 1 {
		t.Fatalf("manifests after adopt %+v", mans)
	}
	complete(t, m2)
	if _, err := Adopt(sysfs.OS{}, t.TempDir(), "bb", "h2"); !errors.Is(err, ErrNotMirror) {
		t.Fatalf("adopt of an empty dir: %v", err)
	}
}
