// Package mirror is the on-disk side of snapshots (docs/16): a mirror disk holds whole
// chunks under their hash, canonical directory trees under theirs and one manifest per
// snapshot, written so that a manifest on the disk always names a complete snapshot. It
// needs nothing from the cluster to be read (docs/16 §2, §7).
package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// Format is the version of the layout this package writes.
const Format = 1

// MarkerFile names the mirror's identity file at its root.
const MarkerFile = "mirror_id"

// Errors.
var (
	ErrNotMirror = errors.New("mirror: no mirror marker at this path")
	ErrForeign   = errors.New("mirror: the marker names another cluster")
	ErrNotEmpty  = errors.New("mirror: the path holds something other than a mirror")
	ErrCorrupt   = errors.New("mirror: corrupt object or tree")
	ErrNotFound  = errors.New("mirror: not on this mirror")
)

// Marker is the mirror's identity (docs/16 §2).
type Marker struct {
	Format    int    `json:"format"`
	ClusterID string `json:"cluster_id"`
	MirrorID  string `json:"mirror_id"`
	HostID    string `json:"host_id"`
	Created   int64  `json:"created"`
}

// Manifest describes one snapshot on the mirror; it is written last (docs/16 §2, §3).
type Manifest struct {
	Format    int       `json:"format"`
	ClusterID string    `json:"cluster_id"`
	ID        uint32    `json:"id"`
	Name      string    `json:"name"`
	Index     uint64    `json:"index"`
	At        int64     `json:"at"`
	Auto      bool      `json:"auto"`
	Root      HexHash   `json:"root"`
	ChunkSize uint32    `json:"chunk_size"`
	Files     uint64    `json:"files"`
	Dirs      uint64    `json:"dirs"`
	Bytes     uint64    `json:"bytes"`
	NewBytes  uint64    `json:"new_bytes"`
	Missing   []HexHash `json:"missing,omitempty"`
}

// Mirror is an opened mirror disk. Puts run concurrently with each other; a sweep
// excludes them, so it never deletes what a running write is about to reference.
type Mirror struct {
	fsys   sysfs.FS
	root   string
	marker Marker

	sweep   sync.RWMutex // puts hold it shared, Sweep exclusively
	mu      sync.Mutex
	objects map[meta.Hash]int64 // object → stored size
	trees   map[meta.Hash]int64
	active  map[*Writer]struct{} // writes in progress: their references are marked by Sweep
	dirs    map[string]bool
}

// Create marks an empty (or absent) directory as a mirror. Anything else in it is
// ErrNotEmpty: a mirror never takes over a directory that holds data.
func Create(fsys sysfs.FS, root string, m Marker) error {
	if ents, err := fsys.ReadDir(root); err == nil {
		for _, e := range ents {
			if e.Name() != "lost+found" {
				return fmt.Errorf("%w: %s", ErrNotEmpty, root)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if m.Format == 0 {
		m.Format = Format
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := fsys.MkdirAll(filepath.Join(root, "tmp"), 0o700); err != nil {
		return err
	}
	return writeDurable(fsys, filepath.Join(root, "tmp"), filepath.Join(root, MarkerFile), b)
}

// ReadMarker reads a mirror's identity without opening it.
func ReadMarker(fsys sysfs.FS, root string) (Marker, error) {
	b, err := readAll(fsys, filepath.Join(root, MarkerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, ErrNotMirror
	}
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil || m.MirrorID == "" {
		return Marker{}, fmt.Errorf("%w: unreadable marker", ErrNotMirror)
	}
	if m.Format != Format {
		return Marker{}, fmt.Errorf("mirror: format %d, this binary reads %d", m.Format, Format)
	}
	return m, nil
}

// Open opens the mirror at root for a cluster (clusterID in hex), removing the temps an
// interrupted write left and indexing its objects and trees. A marker of another
// cluster is ErrForeign, and nothing is touched (I-183). An empty clusterID opens any
// mirror (the standalone readers, docs/16 §7) and then nothing is written either.
func Open(fsys sysfs.FS, root, clusterID string) (*Mirror, error) {
	mk, err := ReadMarker(fsys, root)
	if err != nil {
		return nil, err
	}
	if clusterID != "" && !strings.EqualFold(mk.ClusterID, clusterID) {
		return nil, fmt.Errorf("%w: %s", ErrForeign, mk.ClusterID)
	}
	m := &Mirror{
		fsys: fsys, root: root, marker: mk, objects: map[meta.Hash]int64{}, trees: map[meta.Hash]int64{},
		active: map[*Writer]struct{}{}, dirs: map[string]bool{},
	}
	if clusterID != "" {
		if ents, err := fsys.ReadDir(m.tmpDir()); err == nil {
			for _, e := range ents {
				_ = fsys.Remove(filepath.Join(m.tmpDir(), e.Name())) // an interrupted write: never named by anything (I-180)
			}
		}
	}
	for dir, idx := range map[string]map[meta.Hash]int64{"objects": m.objects, "trees": m.trees} {
		if err := walkTwoLevel(fsys, filepath.Join(root, dir), func(name string, size int64) {
			if h, ok := parseHash(name); ok {
				idx[h] = size
			}
		}); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return m, nil
}

// Marker returns the mirror's identity.
func (m *Mirror) Marker() Marker { return m.marker }

// Root is the mirror's path.
func (m *Mirror) Root() string { return m.root }

func (m *Mirror) tmpDir() string { return filepath.Join(m.root, "tmp") }

func (m *Mirror) path(kind string, h meta.Hash) string {
	s := hex.EncodeToString(h[:])
	return filepath.Join(m.root, kind, s[:2], s)
}

// Used is the bytes the mirror's objects and trees take.
func (m *Mirror) Used() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, s := range m.objects {
		n += s
	}
	for _, s := range m.trees {
		n += s
	}
	return n
}

// HasObject reports whether the mirror holds chunk h.
func (m *Mirror) HasObject(h meta.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[h]
	return ok
}

// HasTree reports whether the mirror holds tree h.
func (m *Mirror) HasTree(h meta.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.trees[h]
	return ok
}

// PutObject stores a chunk's bytes under its hash, after checking that they are that
// chunk; an object already there is left alone (added 0). Durable when it returns.
func (m *Mirror) PutObject(h meta.Hash, data []byte) (added int64, err error) {
	sum := sha256.Sum256(data)
	if sum != h {
		return 0, fmt.Errorf("%w: chunk bytes do not hash to %x", ErrCorrupt, h[:4])
	}
	if m.HasObject(h) {
		return 0, nil
	}
	return m.put("objects", h, chunk.Frame(data, sum), m.objects)
}

func (m *Mirror) put(kind string, h meta.Hash, b []byte, idx map[meta.Hash]int64) (int64, error) {
	m.sweep.RLock()
	defer m.sweep.RUnlock()
	final := m.path(kind, h)
	if err := m.mkdir(filepath.Dir(final)); err != nil {
		return 0, err
	}
	if err := writeDurable(m.fsys, m.tmpDir(), final, b); err != nil {
		return 0, err
	}
	m.mu.Lock()
	idx[h] = int64(len(b))
	m.mu.Unlock()
	return int64(len(b)), nil
}

func (m *Mirror) mkdir(dir string) error {
	m.mu.Lock()
	done := m.dirs[dir]
	m.mu.Unlock()
	if done {
		return nil
	}
	if err := m.fsys.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	m.mu.Lock()
	m.dirs[dir] = true
	m.mu.Unlock()
	return nil
}

// Object reads chunk h, verified against its header and its name (I-180).
func (m *Mirror) Object(h meta.Hash) ([]byte, error) {
	raw, err := readAll(m.fsys, m.path("objects", h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: object %x", ErrNotFound, h[:4])
	}
	if err != nil {
		return nil, err
	}
	data, err := chunk.Unframe(raw)
	if err != nil || sha256.Sum256(data) != h {
		return nil, fmt.Errorf("%w: object %x", ErrCorrupt, h[:4])
	}
	return data, nil
}

// Tree reads tree h, verified against its name.
func (m *Mirror) Tree(h meta.Hash) (Tree, error) {
	raw, err := readAll(m.fsys, m.path("trees", h))
	if errors.Is(err, fs.ErrNotExist) {
		return Tree{}, fmt.Errorf("%w: tree %x", ErrNotFound, h[:4])
	}
	if err != nil {
		return Tree{}, err
	}
	if TreeHash(raw) != h {
		return Tree{}, fmt.Errorf("%w: tree %x", ErrCorrupt, h[:4])
	}
	t, err := DecodeTree(raw)
	if err != nil {
		return Tree{}, fmt.Errorf("%w: tree %x: %w", ErrCorrupt, h[:4], err)
	}
	return t, nil
}

func (m *Mirror) manifestPath(id uint32) string {
	return filepath.Join(m.root, "snaps", strconv.FormatUint(uint64(id), 10)+".json")
}

// PutManifest writes a snapshot's manifest: the last write of a sync, after every tree
// and object it names (I-180).
func (m *Mirror) PutManifest(man Manifest) error {
	if man.Format == 0 {
		man.Format = Format
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	m.sweep.RLock()
	defer m.sweep.RUnlock()
	if err := m.mkdir(filepath.Join(m.root, "snaps")); err != nil {
		return err
	}
	return writeDurable(m.fsys, m.tmpDir(), m.manifestPath(man.ID), b)
}

// Manifests lists the snapshots on the mirror in ID order.
func (m *Mirror) Manifests() ([]Manifest, error) {
	ents, err := m.fsys.ReadDir(filepath.Join(m.root, "snaps"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := readAll(m.fsys, filepath.Join(m.root, "snaps", e.Name()))
		if err != nil {
			return nil, err
		}
		var man Manifest
		if err := json.Unmarshal(b, &man); err != nil {
			return nil, fmt.Errorf("mirror: manifest %s: %w", e.Name(), err)
		}
		out = append(out, man)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RemoveManifest takes one snapshot's manifest off the disk; its trees and objects stay
// until a Sweep finds nothing else reaching them.
func (m *Mirror) RemoveManifest(id uint32) error {
	err := m.fsys.Remove(m.manifestPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.fsys.SyncDir(filepath.Join(m.root, "snaps"))
}

// HexHash is a hash written as hex text in a manifest, readable without this binary.
type HexHash meta.Hash

// MarshalText implements encoding.TextMarshaler.
func (h HexHash) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(h[:])), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *HexHash) UnmarshalText(b []byte) error {
	v, ok := parseHash(string(b))
	if !ok {
		return fmt.Errorf("mirror: bad hash %q", b)
	}
	*h = HexHash(v)
	return nil
}

func parseHash(name string) (meta.Hash, bool) {
	var h meta.Hash
	if len(name) != 64 {
		return h, false
	}
	if _, err := hex.Decode(h[:], []byte(name)); err != nil {
		return h, false
	}
	return h, true
}
