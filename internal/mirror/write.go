package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/zombar/tailraid/internal/meta"
)

// Source is the volume as of one snapshot's index (docs/16 §3): the mirror's host
// adapts the FSM clone it captured, and the tests adapt a map.
type Source interface {
	// Dir lists directory ino's entries: a directory entry carries its Ino (its Child is
	// computed here), a file its Size and Chunks, a symlink its Target.
	Dir(ino uint64) ([]Entry, error)
	// Chunk reads chunk h; any error but the context's makes it a missing chunk.
	Chunk(ctx context.Context, h meta.Hash) ([]byte, error)
}

// SyncWorkers is how many chunks a snapshot sync reads and writes at once (docs/16 §3,
// I-239): each costs a round trip to the hosts holding its shards and a durable write to
// the mirror disk, and one at a time a sync moved some 6 MB/s. mirrors.rate still caps
// the bytes a second across them.
const SyncWorkers = 8

// pendingTree is a tree the walk encoded, written once every object is durable.
type pendingTree struct {
	h   meta.Hash
	enc []byte
}

// Writer is one snapshot being written. Everything it references is marked for a sweep
// that runs meanwhile, so the sweep never takes what the coming manifest will name.
type Writer struct {
	m    *Mirror
	src  Source
	mu   sync.Mutex
	refs map[meta.Hash]struct{} // objects and trees this snapshot names so far

	files, dirs, bytes, newBytes uint64 // newBytes under mu: the workers add to it
	missing                      map[meta.Hash]struct{}
	pending                      map[meta.Hash]chan struct{} // chunks a worker is reading and writing, closed when done
	slots                        chan struct{}               // SyncWorkers tokens
	err                          error                       // the first object write that failed, under mu
	objects                      sync.WaitGroup              // object reads and writes in flight
	trees                        []pendingTree               // trees the mirror lacks, children before parents

	chunkSize   uint64
	read, added atomic.Uint64 // progress: file bytes walked, bytes written (Progress)
}

// Progress is how far the snapshot being written to m has come: the file bytes it has
// walked and the bytes it has written to the mirror; false when nothing is being written.
func (m *Mirror) Progress() (read, added uint64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for w := range m.active {
		read, added, ok = read+w.read.Load(), added+w.added.Load(), true
	}
	return read, added, ok
}

// TotalBytes is the file bytes of the tree under directory ino root: what a snapshot of
// src walks, the total its Progress counts towards.
func TotalBytes(src Source, root uint64) (uint64, error) {
	var walk func(ino uint64, depth int) (uint64, error)
	walk = func(ino uint64, depth int) (uint64, error) {
		if depth > maxDepth {
			return 0, errors.New("mirror: directory tree deeper than any path")
		}
		ents, err := src.Dir(ino)
		if err != nil {
			return 0, err
		}
		var n uint64
		for i := range ents {
			switch ents[i].Type {
			case TypeDir:
				sub, err := walk(ents[i].Ino, depth+1)
				if err != nil {
					return 0, err
				}
				n += sub
			case TypeFile:
				n += ents[i].Size
			case TypeSymlink:
			}
		}
		return n, nil
	}
	return walk(root, 0)
}

// WriteSnapshot writes the snapshot of src rooted at directory ino root and its manifest
// (docs/16 §3): only the objects and trees the mirror lacks are written, objects before
// the trees naming them, trees before their parents and the manifest last. A chunk src
// cannot read is listed in Missing and the rest is written. man's ID, Name, Index, At,
// Auto, ClusterID and ChunkSize are kept; the rest is filled in.
func (m *Mirror) WriteSnapshot(ctx context.Context, src Source, root uint64, man Manifest) (Manifest, error) {
	w := &Writer{
		m: m, src: src, refs: map[meta.Hash]struct{}{}, missing: map[meta.Hash]struct{}{},
		pending: map[meta.Hash]chan struct{}{}, slots: make(chan struct{}, SyncWorkers), chunkSize: uint64(man.ChunkSize),
	}
	m.mu.Lock()
	m.active[w] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.active, w)
		m.mu.Unlock()
	}()
	h, err := w.dir(ctx, root, 0)
	w.objects.Wait()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		w.mu.Lock()
		err = w.err
		w.mu.Unlock()
	}
	if err != nil {
		return Manifest{}, err
	}
	// every object is durable: the trees go now, children before their parents, and the
	// manifest after them (I-180)
	for _, t := range w.trees {
		if w.m.HasTree(t.h) {
			continue
		}
		n, err := w.m.put("trees", t.h, t.enc, w.m.trees)
		if err != nil {
			return Manifest{}, err
		}
		w.newBytes += uint64(n)
		w.added.Add(uint64(n))
	}
	man.Root, man.Files, man.Dirs, man.Bytes, man.NewBytes = HexHash(h), w.files, w.dirs, w.bytes, w.newBytes
	man.Missing = nil
	for mh := range w.missing {
		man.Missing = append(man.Missing, HexHash(mh))
	}
	slices.SortFunc(man.Missing, func(a, b HexHash) int { return bytes.Compare(a[:], b[:]) })
	if man.ClusterID == "" {
		man.ClusterID = m.marker.ClusterID
	}
	if err := m.PutManifest(man); err != nil {
		return Manifest{}, err
	}
	return man, nil
}

func (w *Writer) ref(h meta.Hash) {
	w.mu.Lock()
	w.refs[h] = struct{}{}
	w.mu.Unlock()
}

const maxDepth = meta.MaxPathLen // a path of at most MaxPathLen bytes has fewer levels (I-115)

func (w *Writer) dir(ctx context.Context, ino uint64, depth int) (meta.Hash, error) {
	if depth > maxDepth {
		return meta.Hash{}, errors.New("mirror: directory tree deeper than any path")
	}
	ents, err := w.src.Dir(ino)
	if err != nil {
		return meta.Hash{}, fmt.Errorf("mirror: list directory %d: %w", ino, err)
	}
	// the directory's objects go to the workers and the walk goes on; its tree's hash
	// needs only the entries, and the tree itself is written after every object (I-180)
	for i := range ents {
		e := &ents[i]
		switch e.Type {
		case TypeDir:
			if e.Child, err = w.dir(ctx, e.Ino, depth+1); err != nil {
				return meta.Hash{}, err
			}
		case TypeFile:
			if err := w.file(ctx, e); err != nil {
				return meta.Hash{}, err
			}
		case TypeSymlink:
		}
	}
	w.dirs++
	enc, err := EncodeTree(Tree{Entries: ents})
	if err != nil {
		return meta.Hash{}, err
	}
	h := TreeHash(enc)
	w.ref(h) // before the check: a sweep from now on keeps it
	if w.m.HasTree(h) {
		return h, nil
	}
	w.trees = append(w.trees, pendingTree{h: h, enc: enc})
	return h, nil
}

// file hands e's chunks the mirror lacks to the workers, at most SyncWorkers at once
// (I-239). A chunk another file of this sync is already on is not read twice.
func (w *Writer) file(ctx context.Context, e *Entry) error {
	w.files++
	w.bytes += e.Size
	left := e.Size
	for _, h := range e.Chunks {
		step := left
		if w.chunkSize > 0 {
			step = min(left, w.chunkSize)
		}
		left -= step
		if h == meta.HashZero {
			w.read.Add(step)
			continue
		}
		w.ref(h)
		if w.m.HasObject(h) {
			w.read.Add(step)
			continue
		}
		w.mu.Lock()
		_, gone := w.missing[h]
		done, busy := w.pending[h]
		if !gone && !busy {
			done = make(chan struct{})
			w.pending[h] = done
		}
		w.mu.Unlock()
		if gone || busy {
			w.read.Add(step)
			continue
		}
		select {
		case w.slots <- struct{}{}:
		case <-ctx.Done():
			close(done)
			return ctx.Err()
		}
		w.objects.Go(func() {
			defer func() { <-w.slots }()
			defer close(done)
			w.object(ctx, h)
			w.read.Add(step) // progress counts a chunk once it is handled, not when the walk passes it
		})
	}
	w.read.Add(left) // a file with no chunk list (size 0, or all holes past the list)
	return nil
}

// object reads chunk h, verifies it and writes it to the mirror, or records it missing.
func (w *Writer) object(ctx context.Context, h meta.Hash) {
	data, err := w.src.Chunk(ctx, h)
	if ctx.Err() != nil {
		return
	}
	if err != nil || sha256.Sum256(data) != h {
		w.mu.Lock()
		w.missing[h] = struct{}{} // unreadable now; the next sync tries again (L-215)
		w.mu.Unlock()
		return
	}
	n, err := w.m.PutObject(h, data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if w.err == nil {
			w.err = err
		}
		return
	}
	w.newBytes += uint64(n)
	w.added.Add(uint64(n))
}

// marks collects every object and tree a manifest reaches, plus every reference of a
// write in progress. A manifest whose tree cannot be read stops the sweep: what lies
// under a lost tree is not known, so nothing may be taken.
func (m *Mirror) marks() (map[meta.Hash]struct{}, error) {
	mans, err := m.Manifests()
	if err != nil {
		return nil, err
	}
	keep := map[meta.Hash]struct{}{}
	var walk func(h meta.Hash) error
	walk = func(h meta.Hash) error {
		if _, done := keep[h]; done {
			return nil
		}
		keep[h] = struct{}{}
		t, err := m.Tree(h)
		if err != nil {
			return err
		}
		for i := range t.Entries {
			e := &t.Entries[i]
			switch e.Type {
			case TypeDir:
				if err := walk(e.Child); err != nil {
					return err
				}
			case TypeFile:
				for _, c := range e.Chunks {
					keep[c] = struct{}{}
				}
			case TypeSymlink:
			}
		}
		return nil
	}
	for i := range mans {
		man := &mans[i]
		if err := walk(meta.Hash(man.Root)); err != nil {
			return nil, fmt.Errorf("mirror: snapshot %d: %w", man.ID, err)
		}
	}
	m.mu.Lock()
	for w := range m.active {
		w.mu.Lock()
		for h := range w.refs {
			keep[h] = struct{}{}
		}
		w.mu.Unlock()
	}
	m.mu.Unlock()
	return keep, nil
}

// Sweep removes every object and tree no manifest and no write in progress reaches, and
// never one that they reach (I-183). Writes wait while it runs.
func (m *Mirror) Sweep() (removed int, freed int64, err error) {
	m.sweep.Lock()
	defer m.sweep.Unlock()
	keep, err := m.marks()
	if err != nil {
		return 0, 0, err
	}
	for kind, idx := range map[string]map[meta.Hash]int64{"objects": m.objects, "trees": m.trees} {
		m.mu.Lock()
		var gone []meta.Hash
		for h := range idx {
			if _, ok := keep[h]; !ok {
				gone = append(gone, h)
			}
		}
		m.mu.Unlock()
		dirs := map[string]bool{}
		for _, h := range gone {
			p := m.path(kind, h)
			if err := m.fsys.Remove(p); err != nil {
				return removed, freed, err
			}
			dirs[filepath.Dir(p)] = true
			m.mu.Lock()
			freed += idx[h]
			delete(idx, h)
			m.mu.Unlock()
			removed++
		}
		for d := range dirs {
			if err := m.fsys.SyncDir(d); err != nil {
				return removed, freed, err
			}
		}
	}
	return removed, freed, nil
}

// Row is what the mirror's host needs of a catalogue row of its mirror.
type Row struct {
	ID       uint32
	Deleting bool
}

// Plan is the reconciliation of a mirror's disk with the catalogue (docs/16 §5): the
// manifests of Deleting rows are removed, and a manifest with no row is imported, never
// removed (a forgotten snapshot comes back, I-183).
func Plan(rows []Row, mans []Manifest) (remove []uint32, imported []Manifest) {
	byID := make(map[uint32]Row, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	for i := range mans {
		man := &mans[i]
		r, ok := byID[man.ID]
		switch {
		case !ok:
			imported = append(imported, *man)
		case r.Deleting:
			remove = append(remove, man.ID)
		}
	}
	return remove, imported
}

// Reconcile applies Plan on the disk: it removes the planned manifests, sweeps, and
// returns the IDs it removed (whose rows the host now ends with SnapshotDelete{Done}) and
// the manifests to import. Rows marked Deleting whose manifest is already gone are
// returned as removed too, so a crash between the sweep and the proposal is harmless.
func (m *Mirror) Reconcile(rows []Row) (removed []uint32, imported []Manifest, freed int64, err error) {
	mans, err := m.Manifests()
	if err != nil {
		return nil, nil, 0, err
	}
	remove, imported := Plan(rows, mans)
	for _, id := range remove {
		if err := m.RemoveManifest(id); err != nil {
			return nil, nil, 0, err
		}
	}
	if _, freed, err = m.Sweep(); err != nil {
		return nil, nil, 0, err
	}
	for _, r := range rows {
		if r.Deleting {
			removed = append(removed, r.ID)
		}
	}
	return removed, imported, freed, nil
}
