package testharness

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/sysfs"
)

// Mirrors is an in-process vfs.MirrorSource over the mirrors a Fabric added (docs/16
// §6): every mirror is on this host, and a test takes one offline with SetOnline.
type Mirrors struct {
	mu      sync.Mutex
	ms      map[meta.NodeIdx]*mirror.Mirror
	offline map[meta.NodeIdx]bool
	objects atomic.Int64
}

// NewMirrors returns an empty source.
func NewMirrors() *Mirrors {
	return &Mirrors{ms: map[meta.NodeIdx]*mirror.Mirror{}, offline: map[meta.NodeIdx]bool{}}
}

func (s *Mirrors) get(m meta.NodeIdx) (*mirror.Mirror, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mm, ok := s.ms[m]
	if !ok || s.offline[m] {
		return nil, fmt.Errorf("mirror %d is offline", m)
	}
	return mm, nil
}

// Tree reads tree h of mirror m.
func (s *Mirrors) Tree(_ context.Context, m meta.NodeIdx, h meta.Hash) (mirror.Tree, error) {
	mm, err := s.get(m)
	if err != nil {
		return mirror.Tree{}, err
	}
	return mm.Tree(h)
}

// Object reads chunk object h of mirror m and counts it.
func (s *Mirrors) Object(_ context.Context, m meta.NodeIdx, h meta.Hash) ([]byte, error) {
	mm, err := s.get(m)
	if err != nil {
		return nil, err
	}
	s.objects.Add(1)
	return mm.Object(h)
}

// Online reports whether mirror m is attached.
func (s *Mirrors) Online(m meta.NodeIdx) bool {
	_, err := s.get(m)
	return err == nil
}

// SetOnline attaches or detaches mirror m.
func (s *Mirrors) SetOnline(m meta.NodeIdx, online bool) {
	s.mu.Lock()
	s.offline[m] = !online
	s.mu.Unlock()
}

// Objects is how many objects were read from the mirrors.
func (s *Mirrors) Objects() int64 { return s.objects.Load() }

// Mirror is mirror m's store.
func (s *Mirrors) Mirror(m meta.NodeIdx) *mirror.Mirror {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ms[m]
}

// AddMirror registers a mirror row named name on the fabric's own device and creates its
// disk in a temp dir.
func (f *Fabric) AddMirror(src *Mirrors, name string) meta.NodeIdx {
	f.TB.Helper()
	id := fmt.Sprintf("mirror-%s", filepath.Base(name))
	r := f.Apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: id, Name: name, Kind: meta.KindMirror, Host: f.SelfIdx}}})
	idx := meta.NodeIdx(r.Ino)
	dir := filepath.Join(f.TB.TempDir(), "mirror")
	if err := mirror.Create(sysfs.OS{}, dir, mirror.Marker{Format: mirror.Format, ClusterID: "fabric", MirrorID: id}); err != nil {
		f.TB.Fatal(err)
	}
	mm, err := mirror.Open(sysfs.OS{}, dir, "fabric")
	if err != nil {
		f.TB.Fatal(err)
	}
	src.mu.Lock()
	src.ms[idx] = mm
	src.mu.Unlock()
	return idx
}

// TakeSnapshot takes snapshot name of the volume as it is now on mirror m, the way the
// mirror's host does (docs/16 §3): a SnapshotBegin, the clone at its index written
// through the reader, and a SnapshotEnd.
func (f *Fabric) TakeSnapshot(src *Mirrors, m meta.NodeIdx, name string) meta.Snapshot {
	f.TB.Helper()
	var clone *meta.State
	f.FSM.SetSnapshotHook(func(_ meta.Snapshot, _ meta.NodeIdx, c func() *meta.State) { clone = c() })
	r := f.Apply(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: m, Name: name, Now: f.Clk.Now().UnixNano()}})
	f.FSM.SetSnapshotHook(nil)
	if r.Err != nil || clone == nil {
		f.TB.Fatalf("snapshot begin %s: %v", name, r.Err)
	}
	sn, _ := f.FSM.SnapshotRow(r.SnapID)
	rd := f.Reader()
	man, err := src.Mirror(m).WriteSnapshot(context.Background(), mirror.StateSource(clone, rd.ReadChunk), uint64(meta.RootIno), mirror.Manifest{
		ID: uint32(sn.ID), Name: sn.Name, Index: sn.Index, At: sn.At, ChunkSize: clone.Vol.ChunkSize,
	})
	if err != nil {
		f.TB.Fatal(err)
	}
	f.MustApply(meta.Command{Op: meta.OpSnapshotEnd, SnapshotEnd: &meta.SnapshotEnd{
		ID: sn.ID, OK: true, Root: meta.Hash(man.Root), Files: man.Files, Bytes: man.Bytes, NewBytes: man.NewBytes, Missing: uint32(len(man.Missing)), Now: f.Clk.Now().UnixNano(),
	}})
	sn, _ = f.FSM.SnapshotRow(r.SnapID)
	return sn
}
