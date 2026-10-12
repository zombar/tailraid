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

// Writer is one snapshot being written. Everything it references is marked for a sweep
// that runs meanwhile, so the sweep never takes what the coming manifest will name.
type Writer struct {
	m    *Mirror
	src  Source
	mu   sync.Mutex
	refs map[meta.Hash]struct{} // objects and trees this snapshot names so far

	files, dirs, bytes, newBytes uint64
	missing                      map[meta.Hash]struct{}
}

// WriteSnapshot writes the snapshot of src rooted at directory ino root and its manifest
// (docs/16 §3): only the objects and trees the mirror lacks are written, objects before
// the trees naming them, trees before their parents and the manifest last. A chunk src
// cannot read is listed in Missing and the rest is written. man's ID, Name, Index, At,
// Auto, ClusterID and ChunkSize are kept; the rest is filled in.
func (m *Mirror) WriteSnapshot(ctx context.Context, src Source, root uint64, man Manifest) (Manifest, error) {
	w := &Writer{m: m, src: src, refs: map[meta.Hash]struct{}{}, missing: map[meta.Hash]struct{}{}}
	m.mu.Lock()
	m.active[w] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.active, w)
		m.mu.Unlock()
	}()
	h, err := w.dir(ctx, root, 0)
	if err != nil {
		return Manifest{}, err
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
	n, err := w.m.put("trees", h, enc, w.m.trees)
	if err != nil {
		return meta.Hash{}, err
	}
	w.newBytes += uint64(n)
	return h, nil
}

func (w *Writer) file(ctx context.Context, e *Entry) error {
	w.files++
	w.bytes += e.Size
	for _, h := range e.Chunks {
		if h == meta.HashZero {
			continue
		}
		w.ref(h)
		if w.m.HasObject(h) {
			continue
		}
		if _, gone := w.missing[h]; gone {
			continue
		}
		data, err := w.src.Chunk(ctx, h)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || sha256.Sum256(data) != h {
			w.missing[h] = struct{}{} // unreadable now; the next sync tries again (L-215)
			continue
		}
		n, err := w.m.PutObject(h, data)
		if err != nil {
			return err
		}
		w.newBytes += uint64(n)
	}
	return nil
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
