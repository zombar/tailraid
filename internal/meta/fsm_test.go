package meta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testseed"

	"github.com/hashicorp/raft"
)

const testCS = 1 << 20

func sha(s string) Hash { return sha256.Sum256([]byte(s)) }

type tfsm struct {
	t   *testing.T
	f   *FSM
	idx uint64
}

func newTFSM(t *testing.T) *tfsm {
	t.Helper()
	tf := &tfsm{t: t, f: NewFSM()}
	tf.ok(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})
	return tf
}

func (tf *tfsm) apply(c Command) Result {
	tf.t.Helper()
	tf.idx++
	res, ok := tf.f.Apply(&raft.Log{Index: tf.idx, Type: raft.LogCommand, Data: EncodeCommand(c)}).(Result)
	if !ok {
		tf.t.Fatalf("Apply returned %T, want Result", res)
	}
	if v := CheckInvariants(tf.f.state()); len(v) != 0 {
		tf.t.Fatalf("invariants violated after op %d at index %d: %v", c.Op, tf.idx, v)
	}
	return res
}

func (tf *tfsm) ok(c Command) Result {
	tf.t.Helper()
	res := tf.apply(c)
	if res.Err != nil {
		tf.t.Fatalf("op %d: unexpected error %v", c.Op, res.Err)
	}
	return res
}

func (tf *tfsm) fails(c Command, want error) {
	tf.t.Helper()
	before := encodeState(tf.t, tf.f.state())
	res := tf.apply(c)
	if !errors.Is(res.Err, want) {
		tf.t.Fatalf("op %d: got error %v, want %v", c.Op, res.Err, want)
	}
	after := encodeState(tf.t, tf.f.state())
	// Applied index advances; everything else must be identical (I-26).
	if !bytes.Equal(before[13:], after[13:]) {
		tf.t.Fatalf("op %d: failed command mutated state", c.Op)
	}
}

func (tf *tfsm) create(parent Ino, name string) Ino {
	tf.t.Helper()
	return tf.ok(Command{Op: OpCreate, Create: &Create{Parent: parent, Name: name, Type: TFile, Mode: 0o644, Now: 1}}).Ino
}

func (tf *tfsm) mkdir(parent Ino, name string) Ino {
	tf.t.Helper()
	return tf.ok(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: parent, Name: name, Mode: 0o755, Now: 1}}).Ino
}

func desc(hh Hash) ChunkDesc {
	return ChunkDesc{Hash: hh, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}}
}

func (tf *tfsm) commit(ino Ino, base, size uint64, writes []ChunkWrite, news []ChunkDesc) Result {
	tf.t.Helper()
	return tf.apply(Command{Op: OpCommit, Commit: &Commit{Ino: ino, BaseVersion: base, NewSize: size, Writes: writes, NewChunks: news, Now: 100}})
}

func encodeState(t *testing.T, s *State) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := EncodeState(&buf, s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (tf *tfsm) inode(ino Ino) Inode {
	tf.t.Helper()
	in, ok := tf.f.GetInode(ino)
	if !ok {
		tf.t.Fatalf("inode %d missing", ino)
	}
	return in
}

// child resolves name in dir, or 0.
func (tf *tfsm) child(dir Ino, name string) Ino {
	tf.t.Helper()
	in, ok := tf.f.Lookup(dir, name)
	if !ok {
		return 0
	}
	return in.Ino
}

func (tf *tfsm) chunk(hh Hash) Chunk {
	tf.t.Helper()
	c, ok := tf.f.GetChunk(hh)
	if !ok {
		tf.t.Fatalf("chunk %x missing", hh[:4])
	}
	return c
}

// ---------------------------------------------------------------------------
// Bootstrap, volume, meta, nodes

// Invariant: I-26
func TestFSM_BadCommandLeavesStateUnchanged(t *testing.T) {
	tf := newTFSM(t)
	tf.create(RootIno, "a")
	before := encodeState(t, tf.f.state())
	rng := testseed.Seed(t)
	for range 500 {
		b := make([]byte, rng.Intn(64))
		rng.Read(b)
		tf.idx++
		res := tf.f.Apply(&raft.Log{Index: tf.idx, Type: raft.LogCommand, Data: b}).(Result)
		if !errors.Is(res.Err, ErrBadCommand) {
			t.Fatalf("garbage %x: err %v, want ErrBadCommand", b, res.Err)
		}
	}
	after := encodeState(t, tf.f.state())
	if !bytes.Equal(before[13:], after[13:]) {
		t.Fatal("garbage commands mutated state")
	}
	if tf.f.Applied() != tf.idx {
		t.Fatalf("Applied = %d, want %d", tf.f.Applied(), tf.idx)
	}
}

func TestFSM_NonCommandLogsIgnored(t *testing.T) {
	f := NewFSM()
	if r := f.Apply(&raft.Log{Index: 1, Type: raft.LogNoop}); r != nil {
		t.Fatalf("noop returned %v", r)
	}
	if f.Applied() != 1 {
		t.Fatalf("Applied = %d", f.Applied())
	}
}

// Lifecycle: L-37
func TestFSM_SetVolume_PolicyVersionBumps(t *testing.T) {
	tf := newTFSM(t)
	if v := tf.f.Volume(); v.PolicyVersion != 0 || v.K != 2 || v.M != 1 || v.ChunkSize != testCS {
		t.Fatalf("initial volume %+v", v)
	}
	tf.ok(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 3, M: 2, Now: 2}})
	if v := tf.f.Volume(); v.PolicyVersion != 1 || v.K != 3 || v.M != 2 {
		t.Fatalf("after change %+v", v)
	}
	tf.ok(Command{Op: OpSetVolume, SetVolume: &SetVolume{K: 3, M: 2, Now: 3}}) // no change, ChunkSize 0 accepted
	if v := tf.f.Volume(); v.PolicyVersion != 1 {
		t.Fatalf("unchanged policy bumped: %+v", v)
	}
	tf.fails(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS * 2, K: 3, M: 2, Now: 4}}, ErrPolicy)
	tf.fails(Command{Op: OpSetVolume, SetVolume: &SetVolume{K: 0, M: 2, Now: 4}}, ErrPolicy)
	tf.fails(Command{Op: OpSetVolume, SetVolume: &SetVolume{K: 200, M: 100, Now: 4}}, ErrPolicy)
}

func TestFSM_SetVolume_FirstNeedsChunkSize(t *testing.T) {
	f := &tfsm{t: t, f: NewFSM()}
	f.fails(Command{Op: OpSetVolume, SetVolume: &SetVolume{K: 2, M: 1, Now: 1}}, ErrPolicy)
	f.fails(Command{Op: OpCommit, Commit: &Commit{Ino: 2, Now: 1}}, ErrPolicy) // no volume yet
}

func TestFSM_MetaSet(t *testing.T) {
	tf := newTFSM(t)
	id := [16]byte{1, 2, 3}
	tf.ok(Command{Op: OpMetaSet, MetaSet: &MetaSet{ClusterID: id, JoinToken: "a", CreatedAt: 5}})
	tf.ok(Command{Op: OpMetaSet, MetaSet: &MetaSet{JoinToken: "b", CreatedAt: 9}}) // zero id ok, token rotates
	tf.ok(Command{Op: OpMetaSet, MetaSet: &MetaSet{ClusterID: id, JoinToken: "c"}})
	tf.fails(Command{Op: OpMetaSet, MetaSet: &MetaSet{ClusterID: [16]byte{9}, JoinToken: "d"}}, ErrBadCommand)
	m := tf.f.Meta()
	if m.ClusterID != id || m.JoinToken != "c" || m.CreatedAt != 5 {
		t.Fatalf("meta %+v", m)
	}
}

// Lifecycle: L-93, L-97
func TestFSM_NodeStates(t *testing.T) {
	tf := newTFSM(t)
	r := tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{Name: "a", Kind: KindDevice, Capacity: 10, State: Removed}}})
	if r.Ino != 1 { // allocated NodeIdx is returned in Ino
		t.Fatalf("allocated idx %d", r.Ino)
	}
	n := tf.f.Nodes()
	if len(n) != 1 || n[0].Idx != 1 || n[0].State != Active || n[0].Name != "a" {
		t.Fatalf("nodes %+v", n)
	}
	tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{Idx: 1, Name: "a2", Kind: KindDevice, Capacity: 20, State: Draining}}})
	if n := tf.f.Nodes(); n[0].Capacity != 20 || n[0].Name != "a2" || n[0].State != Active {
		t.Fatalf("upsert changed state or lost fields: %+v", n[0])
	}
	tf.fails(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{Idx: 7, Name: "x"}}}, ErrNoSuchNode)
	tf.fails(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{Name: "bad", Kind: 0}}}, ErrBadCommand)
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 1, State: Draining, Now: 3}})
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 1, State: Active, Now: 4}})
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 1, State: Removed, Now: 5}})
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 1, State: Active, Now: 6}}, ErrBadCommand)
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 1, State: 9, Now: 6}}, ErrBadCommand)
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 5, State: Active, Now: 6}}, ErrNoSuchNode)
	// a Removed node's Idx is never reused
	r2 := tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{Name: "b", Kind: KindS3, Capacity: 1}}})
	if r2.Ino != 2 {
		t.Fatalf("second node idx %d", r2.Ino)
	}
}

// ---------------------------------------------------------------------------
// Namespace

func TestFSM_Create_Mkdir_Errors(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	f := tf.create(d, "f")
	if in, child := tf.inode(RootIno), tf.child(RootIno, "d"); in.Nlink != 3 || child != d {
		t.Fatalf("root after mkdir: %+v", in)
	}
	if in, child := tf.inode(d), tf.child(d, "f"); in.Nlink != 2 || child != f || in.Parent != RootIno || in.Name != "d" {
		t.Fatalf("dir: %+v", in)
	}
	if in := tf.inode(f); in.Type != TFile || in.Mode != 0o644 || in.Nlink != 1 || in.Gen == 0 || in.Parent != d || in.Name != "f" {
		t.Fatalf("file: %+v", in)
	}
	tf.fails(Command{Op: OpCreate, Create: &Create{Parent: d, Name: "f", Type: TFile}}, ErrExist)
	tf.fails(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: d, Name: "f"}}, ErrExist)
	tf.fails(Command{Op: OpCreate, Create: &Create{Parent: f, Name: "x", Type: TFile}}, ErrNotDir)
	tf.fails(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: f, Name: "x"}}, ErrNotDir)
	tf.fails(Command{Op: OpCreate, Create: &Create{Parent: 999, Name: "x", Type: TFile}}, ErrNotExist)
	for _, bad := range []string{"", ".", "..", "a/b"} {
		tf.fails(Command{Op: OpCreate, Create: &Create{Parent: d, Name: bad, Type: TFile}}, ErrBadCommand)
		tf.fails(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: d, Name: bad}}, ErrBadCommand)
	}
	tf.fails(Command{Op: OpCreate, Create: &Create{Parent: d, Name: "dir", Type: TDir}}, ErrBadCommand)
	tf.fails(Command{Op: OpCreate, Create: &Create{Parent: d, Name: "l", Type: TSymlink}}, ErrBadCommand) // no target
	r := tf.ok(Command{Op: OpCreate, Create: &Create{Parent: d, Name: "l", Type: TSymlink, Target: "/etc", Now: 1}})
	if in := tf.inode(r.Ino); in.Type != TSymlink || in.Target != "/etc" || in.Size != 4 {
		t.Fatalf("symlink %+v", in)
	}
}

// Invariant: I-3
// Lifecycle: L-153
func TestFSM_InoGenNotReusedAfterUnlink(t *testing.T) {
	tf := newTFSM(t)
	seen := map[Ino]uint32{}
	for range 50 {
		r := tf.ok(Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: "f", Type: TFile, Now: 1}})
		if g, dup := seen[r.Ino]; dup {
			t.Fatalf("ino %d reused (gen %d then %d)", r.Ino, g, r.Gen)
		}
		if uint64(r.Gen) != tf.idx {
			t.Fatalf("gen %d != log index %d", r.Gen, tf.idx)
		}
		seen[r.Ino] = r.Gen
		tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "f", Now: 2}})
		if _, ok := tf.f.GetInode(r.Ino); ok {
			t.Fatal("unlinked inode still readable")
		}
	}
	// same after snapshot/restore
	snap, _ := tf.f.Snapshot()
	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	g := NewFSM()
	if err := g.Restore(sink.reader()); err != nil {
		t.Fatal(err)
	}
	r := g.Apply(&raft.Log{Index: tf.idx + 1, Type: raft.LogCommand, Data: EncodeCommand(Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: "f", Type: TFile}})}).(Result)
	if _, dup := seen[r.Ino]; dup {
		t.Fatalf("ino %d reused after restore", r.Ino)
	}
}

// Lifecycle: L-14, L-15, L-75
func TestFSM_Unlink_Rmdir(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	a := tf.create(d, "a")
	b := tf.create(d, "b")
	x := sha("x")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: a, NewSize: testCS, Writes: []ChunkWrite{{0, x}}, NewChunks: []ChunkDesc{desc(x)}, Now: 50}})
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: b, NewSize: testCS, Writes: []ChunkWrite{{0, x}}, Now: 51}})
	if c := tf.chunk(x); c.Refs != 2 {
		t.Fatalf("refs %d", c.Refs)
	}
	tf.fails(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "d", Now: 60}}, ErrIsDir)
	tf.fails(Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: d, Name: "a", Now: 60}}, ErrNotDir)
	tf.fails(Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: RootIno, Name: "d", Now: 60}}, ErrNotEmpty)
	tf.fails(Command{Op: OpUnlink, Unlink: &Unlink{Parent: d, Name: "nope", Now: 60}}, ErrNotExist)
	tf.fails(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 404, Name: "nope", Now: 60}}, ErrNotExist)
	tf.fails(Command{Op: OpUnlink, Unlink: &Unlink{Parent: a, Name: "nope", Now: 60}}, ErrNotDir)
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: d, Name: "a", Now: 60}})
	if c := tf.chunk(x); c.Refs != 1 || c.TombstonedAt != 0 {
		t.Fatalf("after first unlink %+v", c)
	}
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: d, Name: "b", Now: 61}})
	if c := tf.chunk(x); c.Refs != 0 || c.TombstonedAt != 61 {
		t.Fatalf("after second unlink %+v", c)
	}
	if _, tomb := tf.f.state().Tombs[x]; !tomb {
		t.Fatal("not in Tombs")
	}
	tf.ok(Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: RootIno, Name: "d", Now: 62}})
	if in := tf.inode(RootIno); in.Nlink != 2 || len(in.Children) != 0 {
		t.Fatalf("root %+v", in)
	}
	tf.fails(Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: RootIno, Name: "", Now: 62}}, ErrBadCommand)
}

// Lifecycle: L-16, L-17
func TestFSM_Rename_Table(t *testing.T) {
	tf := newTFSM(t)
	d1 := tf.mkdir(RootIno, "d1")
	d2 := tf.mkdir(RootIno, "d2")
	sub := tf.mkdir(d1, "sub")
	emptyDir := tf.mkdir(RootIno, "empty")
	f1 := tf.create(d1, "f1")
	f2 := tf.create(d2, "f2")
	x, y := sha("x"), sha("y")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f1, NewSize: testCS, Writes: []ChunkWrite{{0, x}}, NewChunks: []ChunkDesc{desc(x)}, Now: 5}})
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f2, NewSize: testCS, Writes: []ChunkWrite{{0, y}}, NewChunks: []ChunkDesc{desc(y)}, Now: 5}})
	gen1 := tf.inode(f1).Gen

	rn := func(op Ino, on string, np Ino, nn string) Command {
		return Command{Op: OpRename, Rename: &Rename{OldParent: op, OldName: on, NewParent: np, NewName: nn, Now: 70}}
	}
	// errors
	tf.fails(rn(d1, "missing", d2, "z"), ErrNotExist)
	tf.fails(rn(999, "f1", d2, "z"), ErrNotExist)
	tf.fails(rn(d1, "f1", 999, "z"), ErrNotExist)
	tf.fails(rn(d1, "f1", f2, "z"), ErrNotDir)
	tf.fails(rn(d1, "f1", d2, ""), ErrBadCommand)
	tf.fails(rn(d1, "f1", d2, "a/b"), ErrBadCommand)
	tf.fails(rn(d1, "f1", RootIno, "d2"), ErrIsDir)  // file over dir
	tf.fails(rn(RootIno, "d1", d2, "f2"), ErrNotDir) // dir over file
	tf.fails(rn(RootIno, "d1", RootIno, "d2"), ErrNotEmpty)
	tf.fails(rn(RootIno, "d1", sub, "loop"), ErrLoop)
	tf.fails(rn(RootIno, "d1", d1, "loop"), ErrLoop)
	// same path no-op
	tf.ok(rn(d1, "f1", d1, "f1"))
	// cross-dir keeps ino/gen (L-16)
	tf.ok(rn(d1, "f1", d2, "moved"))
	in := tf.inode(f1)
	if in.Gen != gen1 || in.Parent != d2 || in.Name != "moved" || in.Ctime != 70 {
		t.Fatalf("moved %+v", in)
	}
	if tf.child(d1, "f1") != 0 {
		t.Fatal("old entry remains")
	}
	// file over file: dest unlinked, dest refs dropped (L-17)
	tf.ok(rn(d2, "moved", d2, "f2"))
	if _, ok := tf.f.GetInode(f2); ok {
		t.Fatal("replaced dest still exists")
	}
	if c := tf.chunk(y); c.Refs != 0 || c.TombstonedAt != 70 {
		t.Fatalf("dest chunk %+v", c)
	}
	if tf.child(d2, "f2") != f1 {
		t.Fatal("entry does not point at source")
	}
	// dir over empty dir; nlink bookkeeping
	rootN := tf.inode(RootIno).Nlink
	tf.ok(rn(RootIno, "d1", RootIno, "empty"))
	if _, ok := tf.f.GetInode(emptyDir); ok {
		t.Fatal("replaced empty dir still exists")
	}
	if tf.inode(RootIno).Nlink != rootN-1 || tf.child(RootIno, "empty") != d1 {
		t.Fatalf("root %+v", tf.inode(RootIno))
	}
	// dir cross-parent: parents' Nlink adjust
	d2N, d1N := tf.inode(d2).Nlink, tf.inode(d1).Nlink
	tf.ok(rn(d1, "sub", d2, "sub"))
	if tf.inode(d2).Nlink != d2N+1 || tf.inode(d1).Nlink != d1N-1 || tf.inode(sub).Parent != d2 {
		t.Fatal("dir move nlink bookkeeping wrong")
	}
	// symlink over symlink
	l1 := tf.ok(Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: "l1", Type: TSymlink, Target: "a", Now: 1}}).Ino
	tf.ok(Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: "l2", Type: TSymlink, Target: "b", Now: 1}})
	tf.ok(rn(RootIno, "l1", RootIno, "l2"))
	if tf.child(RootIno, "l2") != l1 {
		t.Fatal("symlink rename")
	}
}

func TestFSM_SetAttr(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	tf.ok(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: f, Mask: AttrMode | AttrUid, Mode: 0o600, Uid: 7, Gid: 9, Mtime: 1234, Now: 99}})
	in := tf.inode(f)
	if in.Mode != 0o600 || in.Uid != 7 || in.Gid != 0 || in.Mtime == 1234 || in.Ctime != 99 {
		t.Fatalf("setattr %+v", in)
	}
	tf.ok(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: f, Mask: AttrAtime | AttrMtime | AttrGid, Atime: 5, Mtime: 6, Gid: 3, Now: 100}})
	in = tf.inode(f)
	if in.Atime != 5 || in.Mtime != 6 || in.Gid != 3 || in.Mode != 0o600 {
		t.Fatalf("setattr2 %+v", in)
	}
	tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: 777, Mask: AttrMode}}, ErrNotExist)
	tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: f, Mask: 0x80}}, ErrBadCommand)
}

// ---------------------------------------------------------------------------
// Commit

// Invariant: I-1, I-27
// Lifecycle: L-51, L-19, L-33, L-151
func TestFSM_Commit_NewFileAndOverwrite(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a, b, c := sha("a"), sha("b"), sha("c")
	r := tf.commit(f, 0, 2*testCS+5, []ChunkWrite{{0, a}, {1, b}, {2, c}}, []ChunkDesc{desc(a), desc(b), {Hash: c, Size: 5, K: 1, M: 1, Placement: []NodeIdx{1, 2}}})
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	in := tf.inode(f)
	if in.Size != 2*testCS+5 || in.Version != 1 || len(in.Chunks) != 3 || in.Mtime != 100 || in.Ctime != 100 {
		t.Fatalf("inode %+v", in)
	}
	for _, hh := range []Hash{a, b, c} {
		if ch := tf.chunk(hh); ch.Refs != 1 || ch.TombstonedAt != 0 {
			t.Fatalf("chunk %+v", ch)
		}
	}
	if ch := tf.chunk(c); ch.Size != 5 || ch.K != 1 || ch.M != 1 || len(ch.Placement) != 2 {
		t.Fatalf("small chunk %+v", ch)
	}
	// overwrite middle chunk: refs move b→d; b tombstoned
	d := sha("d")
	old := in.Chunks
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, NewSize: in.Size, Writes: []ChunkWrite{{1, d}}, NewChunks: []ChunkDesc{desc(d)}, Now: 101}})
	in = tf.inode(f)
	if in.Chunks[1] != d || in.Chunks[0] != a || in.Chunks[2] != c || in.Version != 2 {
		t.Fatalf("after overwrite %+v", in.Chunks)
	}
	if old[1] != b {
		t.Fatal("old chunk slice was mutated in place")
	}
	if ch := tf.chunk(b); ch.Refs != 0 || ch.TombstonedAt != 101 {
		t.Fatalf("b %+v", ch)
	}
	// duplicate chunk inside one file holds 2 refs
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 2, NewSize: in.Size, Writes: []ChunkWrite{{0, d}}, Now: 102}})
	if ch := tf.chunk(d); ch.Refs != 2 {
		t.Fatalf("dup within file refs %d", ch.Refs)
	}
	if ch := tf.chunk(a); ch.Refs != 0 {
		t.Fatalf("a refs %d", ch.Refs)
	}
	// NewChunks for an existing hash: Size must match; (K,M,Placement) ignored
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 3, NewSize: in.Size, NewChunks: []ChunkDesc{{Hash: d, Size: 7, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}}}, Now: 103}}, ErrBadCommand)
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 3, NewSize: in.Size, NewChunks: []ChunkDesc{{Hash: d, Size: testCS, K: 9, M: 9, Placement: []NodeIdx{5}}}, Now: 103}})
	if ch := tf.chunk(d); ch.K != 2 || len(ch.Placement) != 3 {
		t.Fatalf("existing entry altered by NewChunks: %+v", ch)
	}
}

// Invariant: I-28
// Lifecycle: L-53
func TestFSM_Commit_RejectsStaleBaseVersion(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 0, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	b := sha("b")
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 0, NewSize: testCS, Writes: []ChunkWrite{{0, b}}, NewChunks: []ChunkDesc{desc(b)}, Now: 2}}, ErrConflict)
	if _, ok := tf.f.GetChunk(b); ok {
		t.Fatal("conflicting commit registered a chunk")
	}
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 5, NewSize: testCS, Now: 2}}, ErrConflict)
}

// Names are 1..MaxNameLen bytes with no '/' or NUL; too long is its own error so the
// gateway answers NFS3ERR_NAMETOOLONG. Create, Mkdir, Symlink and Rename share the rule.
// Invariant: I-26, I-66
func TestFSM_Create_NameBoundaries(t *testing.T) {
	long := strings.Repeat("a", MaxNameLen)
	multibyte := strings.Repeat("é", MaxNameLen/2) + "a" // 255 bytes of UTF-8
	cases := []struct {
		name string
		val  string
		want error
	}{
		{"empty", "", ErrBadCommand},
		{"one byte", "a", nil},
		{"max", long, nil},
		{"max+1", long + "a", ErrNameTooLong},
		{"utf-8 at max", multibyte, nil},
		{"utf-8 past max", multibyte + "é", ErrNameTooLong},
		{"dot", ".", ErrBadCommand},
		{"dotdot", "..", ErrBadCommand},
		{"slash", "a/b", ErrBadCommand},
		{"nul", "a\x00b", ErrBadCommand},
		{"beyond the wire string limit: undecodable", strings.Repeat("x", 70000), ErrBadCommand},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tf := newTFSM(t)
			d := tf.mkdir(RootIno, "d")
			for i, kind := range []string{"file", "dir", "symlink"} {
				sub := tf.mkdir(d, fmt.Sprintf("op%d", i))
				var cmd Command
				switch kind {
				case "file":
					cmd = Command{Op: OpCreate, Create: &Create{Parent: sub, Name: c.val, Type: TFile, Mode: 0o644, Now: 1}}
				case "dir":
					cmd = Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: sub, Name: c.val, Mode: 0o755, Now: 1}}
				default:
					cmd = Command{Op: OpCreate, Create: &Create{Parent: sub, Name: c.val, Type: TSymlink, Mode: 0o777, Target: "/t", Now: 1}}
				}
				if c.want != nil {
					tf.fails(cmd, c.want)
				} else {
					tf.ok(cmd)
				}
			}
			tf.create(d, "src")
			rn := Command{Op: OpRename, Rename: &Rename{OldParent: d, OldName: "src", NewParent: d, NewName: c.val, Now: 2}}
			if c.want != nil {
				tf.fails(rn, c.want)
			} else {
				tf.ok(rn)
			}
		})
	}
}

// The chunk size is bounded once (I-67): init, the FSM and the shard store share it.
// Invariant: I-67
func TestFSM_SetVolume_ChunkSizeBounds(t *testing.T) {
	cases := []struct {
		cs uint32
		ok bool
	}{
		{0, false}, {1, true}, {MaxChunkSize - 1, true}, {MaxChunkSize, true}, {MaxChunkSize + 1, false}, {1 << 30, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.cs), func(t *testing.T) {
			tf := &tfsm{t: t, f: NewFSM()}
			cmd := Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: c.cs, K: 2, M: 1, Now: 1}}
			if c.ok {
				tf.ok(cmd)
				if v := tf.f.Volume(); v.ChunkSize != c.cs {
					t.Fatalf("chunk size %d", v.ChunkSize)
				}
			} else {
				tf.fails(cmd, ErrPolicy)
			}
		})
	}
}

// The FSM, not the leader, refuses to remove a node that still holds a placement slot
// (live or tombstoned chunk); once the slots are zeroed the removal applies, and a removed
// node can neither change state again nor be removed twice.
// Invariant: I-9, I-26
// Lifecycle: L-95, L-96
func TestFSM_NodeRemove_RefusedWhileHoldingSlots(t *testing.T) {
	tf := newTFSM(t)
	for i := 1; i <= 3; i++ {
		tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: fmt.Sprintf("n%d", i), Kind: KindDevice, Capacity: 1, Addr: "1.1.1.1"}}})
	}
	f := tf.create(RootIno, "f")
	h := sha("h")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 1}})
	remove := Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 2, State: Removed, Now: 2}}
	tf.fails(remove, ErrNodeNotEmpty)
	// a tombstoned chunk still pins the slot
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "f", Now: 3}})
	tf.fails(remove, ErrNodeNotEmpty)
	// draining is allowed while holding slots; removal only once the slot is zeroed
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 2, State: Draining, Now: 4}})
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: h, K: 2, M: 1, Placement: []NodeIdx{1, 0, 3}}})
	tf.ok(remove)
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 2, State: Active, Now: 5}}, ErrBadCommand)
	tf.fails(remove, ErrBadCommand)
	// an empty node leaves straight from Active
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: h, K: 2, M: 1, Placement: []NodeIdx{1, 0, 0}}})
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 3, State: Removed, Now: 6}})
}

// Refs count (inode, index) positions, not files: the same hash at two indices of one
// file counts twice, a truncate that drops one index decrements once, and the invariant
// checker agrees at every step.
// Invariant: I-27
// Lifecycle: L-150
func TestFSM_Commit_SameHashTwiceInOneFile_RefsPerIndex(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	h := sha("same")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: 2 * testCS, Writes: []ChunkWrite{{0, h}, {1, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 1}})
	if c := tf.chunk(h); c.Refs != 2 {
		t.Fatalf("refs %d after two indices", c.Refs)
	}
	g := tf.create(RootIno, "g")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: g, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, Now: 2}})
	if c := tf.chunk(h); c.Refs != 3 {
		t.Fatalf("refs %d after a third position", c.Refs)
	}
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: testCS, NewSize: testCS, Now: 3}})
	if c := tf.chunk(h); c.Refs != 2 {
		t.Fatalf("refs %d after dropping one index", c.Refs)
	}
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "f", Now: 4}})
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "g", Now: 5}})
	if c := tf.chunk(h); c.Refs != 0 || c.TombstonedAt == 0 {
		t.Fatalf("refs %d tombstoned %d after both unlinks", c.Refs, c.TombstonedAt)
	}
}

// A Commit without HasTruncate may keep or grow a file but never shrink it; the flusher
// always sends an explicit truncation point when the file got shorter.
// Invariant: I-26
// Lifecycle: L-175
func TestFSM_Commit_RejectsImplicitShrink(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS + 10, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	size := uint64(testCS + 10)
	cases := []struct {
		name    string
		newSize uint64
		want    error
	}{
		{"shrink by one", size - 1, ErrBadCommand},
		{"shrink to zero", 0, ErrBadCommand},
		{"same size", size, nil},
		{"grow by one", size + 1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := tf.inode(f)
			cmd := Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: before.Version, NewSize: c.newSize, Now: 2}}
			if c.want != nil {
				tf.fails(cmd, c.want)
				if in := tf.inode(f); in.Size != before.Size || in.Version != before.Version {
					t.Fatalf("rejected commit changed the inode: %+v", in)
				}
				if ch := tf.chunk(a); ch.Refs != 1 {
					t.Fatalf("refs %d", ch.Refs)
				}
				return
			}
			tf.ok(cmd)
			if in := tf.inode(f); in.Size != c.newSize {
				t.Fatalf("size %d want %d", in.Size, c.newSize)
			}
		})
	}
	// the same shrink is fine with an explicit truncate
	in := tf.inode(f)
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: in.Version, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 3}})
	if ch := tf.chunk(a); ch.Refs != 0 {
		t.Fatalf("refs after truncate %d", ch.Refs)
	}
}

// The chunk list of every file lives in memory on every node and a Commit rebuilds it, so
// NewSize is bounded; beyond the bound the command is malformed and nothing is allocated.
// Invariant: I-26, I-64
// Lifecycle: L-182
func TestFSM_Commit_NewSizeBounds(t *testing.T) {
	maxSize := MaxFileSize(testCS)
	cases := []struct {
		name    string
		newSize uint64
		ok      bool
	}{
		{"0", 0, true},
		{"1", 1, true},
		{"CS-1", testCS - 1, true},
		{"CS", testCS, true},
		{"CS+1", testCS + 1, true},
		{"max-1", maxSize - 1, true},
		{"max", maxSize, true},
		{"max+1", maxSize + 1, false},
		{"MaxUint64", math.MaxUint64, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tf := newTFSM(t)
			f := tf.create(RootIno, "f")
			cmd := Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: c.newSize, Now: 1}}
			if !c.ok {
				tf.fails(cmd, ErrBadCommand)
				return
			}
			tf.ok(cmd)
			if in := tf.inode(f); in.Size != c.newSize || len(in.Chunks) != ChunkCount(c.newSize, testCS) {
				t.Fatalf("size %d chunks %d", in.Size, len(in.Chunks))
			}
		})
	}
}

// Invariant: I-1, I-64
func TestChunkCount_NoOverflow(t *testing.T) {
	cases := []struct {
		size uint64
		cs   uint32
		want int
	}{
		{0, 1 << 20, 0},
		{1, 1 << 20, 1},
		{1 << 20, 1 << 20, 1},
		{1<<20 + 1, 1 << 20, 2},
		{math.MaxUint64, 1 << 20, 1 << 44},
		{math.MaxUint64 - 5, 1 << 20, 1 << 44},
		{math.MaxUint64, 1, math.MaxInt},
		{math.MaxUint64, 2, math.MaxInt},
	}
	for _, c := range cases {
		if got := ChunkCount(c.size, c.cs); got != c.want {
			t.Fatalf("ChunkCount(%d, %d) = %d, want %d", c.size, c.cs, got, c.want)
		}
	}
}

// Lifecycle: L-54, L-160
func TestFSM_Commit_UnknownChunkRejected(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, sha("ghost")}}, Now: 1}}, ErrUnknownChunk)
	// tombstoned is fine (revive), collected is not
	g := sha("g")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, g}}, NewChunks: []ChunkDesc{desc(g)}, Now: 1}})
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 2}})
	if ch := tf.chunk(g); ch.Refs != 0 || ch.TombstonedAt != 2 {
		t.Fatalf("g %+v", ch)
	}
	r := tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 3}})
	if len(r.Victims) != 1 || r.Victims[0].Hash != g {
		t.Fatalf("victims %+v", r.Victims)
	}
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 2, NewSize: testCS, Writes: []ChunkWrite{{0, g}}, Now: 4}}, ErrUnknownChunk)
}

// Invariant: I-2
func TestFSM_Commit_Errors(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	f := tf.create(RootIno, "f")
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: 999, NewSize: 1}}, ErrNotExist)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: d, NewSize: 1}}, ErrIsDir)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{5, HashZero}}}}, ErrBadCommand)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, NewChunks: []ChunkDesc{{Hash: HashZero, Size: 1, K: 1, M: 0}}}}, ErrBadCommand)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, NewChunks: []ChunkDesc{{Hash: sha("z"), Size: testCS + 1, K: 1, M: 0, Placement: []NodeIdx{1}}}}}, ErrBadCommand)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, NewChunks: []ChunkDesc{{Hash: sha("z"), Size: 0, K: 1, M: 0, Placement: []NodeIdx{1}}}}}, ErrBadCommand)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, NewChunks: []ChunkDesc{{Hash: sha("z"), Size: 1, K: 0, M: 0, Placement: nil}}}}, ErrBadCommand)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, NewChunks: []ChunkDesc{{Hash: sha("z"), Size: 1, K: 2, M: 1, Placement: []NodeIdx{1, 2}}}}}, ErrBadCommand)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, NewChunks: []ChunkDesc{{Hash: sha("z"), Size: 1, K: 2, M: 1, Placement: []NodeIdx{1, 1, 2}}}}}, ErrBadCommand)
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, NewChunks: []ChunkDesc{{Hash: sha("z"), Size: 1, K: 200, M: 100, Placement: make([]NodeIdx, 300)}}}}, ErrBadCommand)
	// a NewChunks entry registered by a failing commit must not survive
	if _, ok := tf.f.GetChunk(sha("z")); ok {
		t.Fatal("failed commit left a chunk entry")
	}
	// HasTruncate beyond NewSize is malformed
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, HasTruncate: true, TruncateTo: 10, NewSize: 5}}, ErrBadCommand)
}

// Lifecycle: L-8, L-9, L-10, L-56
func TestFSM_Commit_TruncateDown(t *testing.T) {
	setup := func(t *testing.T) (*tfsm, Ino, [3]Hash) {
		tf := newTFSM(t)
		f := tf.create(RootIno, "f")
		hs := [3]Hash{sha("a"), sha("b"), sha("c")}
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: 3 * testCS, Writes: []ChunkWrite{{0, hs[0]}, {1, hs[1]}, {2, hs[2]}}, NewChunks: []ChunkDesc{desc(hs[0]), desc(hs[1]), desc(hs[2])}, Now: 1}})
		return tf, f, hs
	}
	t.Run("mid-chunk without tail rewrite", func(t *testing.T) {
		tf, f, _ := setup(t)
		// without HasTruncate a shrink is malformed outright (L-175); with it the tail rule applies
		tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, NewSize: testCS + 10, Now: 2}}, ErrBadCommand)
		tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: testCS + 10, NewSize: testCS + 10, Now: 2}}, ErrNeedTailRewrite)
	})
	t.Run("mid-chunk with tail rewrite", func(t *testing.T) {
		tf, f, hs := setup(t)
		tail := sha("tail")
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: testCS + 10, NewSize: testCS + 10, Writes: []ChunkWrite{{1, tail}}, NewChunks: []ChunkDesc{{Hash: tail, Size: 10, K: 1, M: 1, Placement: []NodeIdx{1, 2}}}, Now: 2}})
		in := tf.inode(f)
		if len(in.Chunks) != 2 || in.Chunks[1] != tail || in.Size != testCS+10 {
			t.Fatalf("%+v", in)
		}
		if tf.chunk(hs[1]).Refs != 0 || tf.chunk(hs[2]).Refs != 0 || tf.chunk(hs[0]).Refs != 1 {
			t.Fatal("refs after truncate")
		}
	})
	t.Run("mid-chunk with HashZero tail", func(t *testing.T) {
		tf, f, _ := setup(t)
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: testCS + 10, NewSize: testCS + 10, Writes: []ChunkWrite{{1, HashZero}}, Now: 2}})
		if in := tf.inode(f); len(in.Chunks) != 2 || in.Chunks[1] != HashZero {
			t.Fatalf("%+v", in)
		}
	})
	t.Run("aligned", func(t *testing.T) {
		tf, f, hs := setup(t)
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: 2 * testCS, NewSize: 2 * testCS, Now: 2}})
		if in := tf.inode(f); len(in.Chunks) != 2 || tf.chunk(hs[2]).Refs != 0 {
			t.Fatalf("%+v", in)
		}
	})
	t.Run("to zero", func(t *testing.T) {
		tf, f, hs := setup(t)
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 2}})
		if in := tf.inode(f); len(in.Chunks) != 0 || in.Size != 0 {
			t.Fatalf("%+v", in)
		}
		for _, hh := range hs {
			if c := tf.chunk(hh); c.Refs != 0 || c.TombstonedAt != 2 {
				t.Fatalf("%+v", c)
			}
		}
	})
}

// The review's bug #2: a truncate followed by growth must drop the bytes beyond the
// truncation point even though the final size is larger than before.
// Lifecycle: L-8, L-11, L-168
func TestFSM_Commit_TruncateThenGrow_DropsOldBytes(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	hs := []Hash{sha("0"), sha("1"), sha("2"), sha("3")}
	var w []ChunkWrite
	var nd []ChunkDesc
	for i, hh := range hs {
		w = append(w, ChunkWrite{uint32(i), hh})
		nd = append(nd, desc(hh))
	}
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: 4 * testCS, Writes: w, NewChunks: nd, Now: 1}})
	// truncate(1.5 chunks) then write @ chunk 5: tail of chunk 1 rewritten, chunks 2,3 dropped,
	// chunk 4 is a hole, chunk 5 new.
	tail, five := sha("tail"), sha("five")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{
		Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: testCS + testCS/2, NewSize: 6 * testCS,
		Writes: []ChunkWrite{{1, tail}, {5, five}}, NewChunks: []ChunkDesc{desc(tail), desc(five)}, Now: 2,
	}})
	in := tf.inode(f)
	want := []Hash{hs[0], tail, HashZero, HashZero, HashZero, five}
	if len(in.Chunks) != 6 {
		t.Fatalf("len %d", len(in.Chunks))
	}
	for i := range want {
		if in.Chunks[i] != want[i] {
			t.Fatalf("chunk %d = %x, want %x", i, in.Chunks[i][:4], want[i][:4])
		}
	}
	for _, hh := range hs[1:] {
		if c := tf.chunk(hh); c.Refs != 0 {
			t.Fatalf("old chunk %x still referenced", hh[:4])
		}
	}
	// same without HasTruncate: growth never drops, and the rebase rule keeps old bytes
	tf2 := newTFSM(t)
	f2 := tf2.create(RootIno, "f")
	tf2.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f2, NewSize: 4 * testCS, Writes: w, NewChunks: nd, Now: 1}})
	tf2.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f2, BaseVersion: 1, NewSize: 6 * testCS, Writes: []ChunkWrite{{5, five}}, NewChunks: []ChunkDesc{desc(five)}, Now: 2}})
	if in := tf2.inode(f2); in.Chunks[2] != hs[2] || in.Chunks[4] != HashZero {
		t.Fatalf("%+v", in.Chunks)
	}
}

// Lifecycle: L-4, L-11
func TestFSM_Commit_GrowWithHoles(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: 3*testCS + 1, Writes: []ChunkWrite{{3, a}}, NewChunks: []ChunkDesc{{Hash: a, Size: 1, K: 1, M: 1, Placement: []NodeIdx{1, 2}}}, Now: 1}})
	in := tf.inode(f)
	if len(in.Chunks) != 4 || in.Chunks[0] != HashZero || in.Chunks[2] != HashZero || in.Chunks[3] != a {
		t.Fatalf("%+v", in.Chunks)
	}
	if _, ok := tf.f.GetChunk(HashZero); ok {
		t.Fatal("HashZero entry created")
	}
	// truncate up keeps short tail; new indices are holes
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, NewSize: 5 * testCS, Now: 2}})
	in = tf.inode(f)
	if len(in.Chunks) != 5 || in.Chunks[3] != a || in.Chunks[4] != HashZero {
		t.Fatalf("%+v", in.Chunks)
	}
}

// Invariant: I-27, I-41
// Lifecycle: L-20, L-34, L-78
func TestFSM_Revive(t *testing.T) {
	tf := newTFSM(t)
	f1 := tf.create(RootIno, "f1")
	f2 := tf.create(RootIno, "f2")
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f1, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "f1", Now: 5}})
	if c := tf.chunk(a); c.Refs != 0 || c.TombstonedAt != 5 {
		t.Fatalf("%+v", c)
	}
	// dedup against a tombstoned chunk revives it; NewChunks may or may not mention it
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f2, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, Now: 6}})
	c := tf.chunk(a)
	if c.Refs != 1 || c.TombstonedAt != 0 || len(c.Placement) != 3 {
		t.Fatalf("revived %+v", c)
	}
	if _, tomb := tf.f.state().Tombs[a]; tomb {
		t.Fatal("still in Tombs")
	}
}

// Lifecycle: L-76, L-77, L-141
func TestFSM_GCDelete_Boundary(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	live, t1, t2 := sha("live"), sha("t1"), sha("t2")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: 3 * testCS, Writes: []ChunkWrite{{0, live}, {1, t1}, {2, t2}}, NewChunks: []ChunkDesc{desc(live), desc(t1), desc(t2)}, Now: 1}})
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: 2 * testCS, NewSize: 2 * testCS, Now: 10}}) // t2 tombstoned at 10
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 2, HasTruncate: true, TruncateTo: testCS, NewSize: testCS, Now: 20}})         // t1 tombstoned at 20
	r := tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 10}})
	if len(r.Victims) != 0 {
		t.Fatalf("Before == TombstonedAt collected: %+v", r.Victims)
	}
	r = tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 11}})
	if len(r.Victims) != 1 || r.Victims[0].Hash != t2 || len(r.Victims[0].Placement) != 3 {
		t.Fatalf("victims %+v", r.Victims)
	}
	if _, ok := tf.f.GetChunk(t2); ok {
		t.Fatal("collected chunk still present")
	}
	if _, ok := tf.f.GetChunk(t1); !ok {
		t.Fatal("young tombstone collected")
	}
	r = tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 1 << 62}})
	if len(r.Victims) != 1 || r.Victims[0].Hash != t1 {
		t.Fatalf("victims %+v", r.Victims)
	}
	if _, ok := tf.f.GetChunk(live); !ok {
		t.Fatal("referenced chunk collected")
	}
	// victims are sorted by hash for determinism
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 3, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 30}})
	g := tf.create(RootIno, "g")
	var ws []ChunkWrite
	var ds []ChunkDesc
	for i := range 20 {
		hh := sha(string(rune('A' + i)))
		ws = append(ws, ChunkWrite{uint32(i), hh})
		ds = append(ds, desc(hh))
	}
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: g, NewSize: 20 * testCS, Writes: ws, NewChunks: ds, Now: 31}})
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "g", Now: 32}})
	r = tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 100}})
	if len(r.Victims) != 21 {
		t.Fatalf("victims %d", len(r.Victims))
	}
	for i := 1; i < len(r.Victims); i++ {
		if bytes.Compare(r.Victims[i-1].Hash[:], r.Victims[i].Hash[:]) >= 0 {
			t.Fatal("victims not sorted")
		}
	}
}

// Lifecycle: L-89, L-100, L-144, L-147
func TestFSM_SetPlacement(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	tf.fails(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: sha("nope"), K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}}}, ErrUnknownChunk)
	tf.fails(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: a, K: 2, M: 1, Placement: []NodeIdx{1, 2}}}, ErrBadCommand)
	tf.fails(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: a, K: 2, M: 1, Placement: []NodeIdx{1, 2, 2}}}, ErrBadCommand)
	tf.fails(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: a, K: 0, M: 1, Placement: []NodeIdx{1}}}, ErrBadCommand)
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: a, K: 3, M: 2, Placement: []NodeIdx{4, 5, 0, 6, 0}}})
	c := tf.chunk(a)
	if c.K != 3 || c.M != 2 || len(c.Placement) != 5 || c.Placement[2] != 0 || c.Refs != 1 {
		t.Fatalf("%+v", c)
	}
	// membership changes never touch placement
	tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{Name: "n", Kind: KindDevice, Capacity: 5}}})
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 1, State: Removed, Now: 9}})
	if c2 := tf.chunk(a); !equalIdx(c2.Placement, c.Placement) {
		t.Fatal("placement changed by membership command")
	}
}

// Lifecycle: L-143
func TestFSM_RefCountNeverUnderflows(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	// corrupt the state behind the FSM's back so that a legitimate command would underflow
	tf.f.mu.Lock()
	tf.f.st.Chunks[a].Refs = 0
	tf.f.mu.Unlock()
	tf.idx++
	res := tf.f.Apply(&raft.Log{Index: tf.idx, Type: raft.LogCommand, Data: EncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "f", Now: 2}})}).(Result)
	if !errors.Is(res.Err, ErrBadCommand) {
		t.Fatalf("underflow produced %v", res.Err)
	}
	if _, ok := tf.f.GetInode(f); !ok {
		t.Fatal("failed unlink removed the inode")
	}
}

func TestFSM_LastNowMonotonic(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	tf.ok(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: f, Mask: AttrMode, Mode: 1, Now: 1000}})
	if tf.f.LastNow() != 1000 {
		t.Fatalf("LastNow %d", tf.f.LastNow())
	}
	tf.ok(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: f, Mask: AttrMode, Mode: 2, Now: 500}}) // skewed proposer
	if in := tf.inode(f); in.Ctime != 1000 {
		t.Fatalf("Ctime went backwards: %d", in.Ctime)
	}
	if tf.f.LastNow() != 1000 {
		t.Fatalf("LastNow %d", tf.f.LastNow())
	}
	// tombstone time uses the clamped clock so GC grace cannot be defeated by skew
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 1}})
	if c := tf.chunk(a); c.TombstonedAt != 1000 {
		t.Fatalf("TombstonedAt %d", c.TombstonedAt)
	}
}

// ---------------------------------------------------------------------------
// Snapshot / restore / determinism

type memSink struct {
	bytes.Buffer
	closed, cancelled bool
}

func (m *memSink) ID() string    { return "mem" }
func (m *memSink) Cancel() error { m.cancelled = true; return nil }
func (m *memSink) Close() error  { m.closed = true; return nil }
func (m *memSink) reader() *readCloser {
	return &readCloser{Reader: bytes.NewReader(m.Bytes())}
}

type readCloser struct {
	*bytes.Reader
}

func (r *readCloser) Close() error { return nil }

// cmdGen produces valid-or-deliberately-invalid commands against the FSM's own view.
type cmdGen struct {
	rng    *rand.Rand
	f      *FSM
	hashes []Hash
	now    int64
}

func (g *cmdGen) files() (dirs, files []Inode) {
	g.f.mu.RLock()
	defer g.f.mu.RUnlock()
	inos := make([]Ino, 0, len(g.f.st.Inodes))
	for ino := range g.f.st.Inodes {
		inos = append(inos, ino)
	}
	// deterministic order given the seed
	for i := 1; i < len(inos); i++ {
		for j := i; j > 0 && inos[j] < inos[j-1]; j-- {
			inos[j], inos[j-1] = inos[j-1], inos[j]
		}
	}
	for _, ino := range inos {
		in := *g.f.st.Inodes[ino]
		switch in.Type {
		case TDir:
			dirs = append(dirs, in)
		case TFile:
			files = append(files, in)
		case TSymlink:
		}
	}
	return dirs, files
}

func (g *cmdGen) next() Command {
	g.now += int64(g.rng.Intn(3)) - 1 // occasionally skewed backwards
	dirs, files := g.files()
	d := dirs[g.rng.Intn(len(dirs))]
	name := string(rune('a' + g.rng.Intn(6)))
	switch g.rng.Intn(14) {
	case 0:
		return Command{Op: OpCreate, Create: &Create{Parent: d.Ino, Name: name, Type: TFile, Mode: 0o644, Now: g.now}}
	case 1:
		return Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: d.Ino, Name: name, Mode: 0o755, Now: g.now}}
	case 2:
		return Command{Op: OpUnlink, Unlink: &Unlink{Parent: d.Ino, Name: name, Now: g.now}}
	case 3:
		return Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: d.Ino, Name: name, Now: g.now}}
	case 4:
		d2 := dirs[g.rng.Intn(len(dirs))]
		return Command{Op: OpRename, Rename: &Rename{OldParent: d.Ino, OldName: name, NewParent: d2.Ino, NewName: string(rune('a' + g.rng.Intn(6))), Now: g.now}}
	case 5:
		return Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: g.now - int64(g.rng.Intn(20))}}
	case 6:
		if len(g.hashes) == 0 {
			break
		}
		hh := g.hashes[g.rng.Intn(len(g.hashes))]
		p := []NodeIdx{NodeIdx(g.rng.Intn(5) + 1), NodeIdx(g.rng.Intn(5) + 6), 0}
		return Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: hh, K: 2, M: 1, Placement: p}}
	case 7:
		if len(files) == 0 {
			break
		}
		fl := files[g.rng.Intn(len(files))]
		return Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: fl.Ino, Mask: AttrMode | AttrMtime, Mode: uint32(g.rng.Intn(512)), Mtime: g.now, Now: g.now}}
	case 8:
		return Command{Op: OpSetVolume, SetVolume: &SetVolume{K: uint8(2 + g.rng.Intn(2)), M: uint8(1 + g.rng.Intn(2)), Now: g.now}}
	}
	if len(files) == 0 {
		return Command{Op: OpCreate, Create: &Create{Parent: d.Ino, Name: name, Type: TFile, Now: g.now}}
	}
	fl := files[g.rng.Intn(len(files))]
	base := fl.Version
	if g.rng.Intn(10) == 0 {
		base++ // deliberate conflict
	}
	newSize := uint64(g.rng.Intn(6)) * testCS
	if g.rng.Intn(2) == 0 {
		newSize += uint64(g.rng.Intn(testCS))
	}
	c := &Commit{Ino: fl.Ino, BaseVersion: base, NewSize: newSize, Now: g.now}
	if g.rng.Intn(3) == 0 && newSize < fl.Size {
		c.HasTruncate, c.TruncateTo = true, uint64(g.rng.Intn(int(newSize)+1))
	}
	n := ChunkCount(newSize, testCS)
	for i := range n {
		if g.rng.Intn(2) == 0 {
			continue
		}
		var hh Hash
		switch g.rng.Intn(4) {
		case 0:
			hh = HashZero
		case 1:
			if len(g.hashes) > 0 {
				hh = g.hashes[g.rng.Intn(len(g.hashes))]
				if g.rng.Intn(3) == 0 { // sometimes re-register
					c.NewChunks = append(c.NewChunks, ChunkDesc{Hash: hh, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}})
				}
				break
			}
			fallthrough
		default:
			hh = sha(string(rune('A' + g.rng.Intn(40))))
			size := uint32(testCS)
			if i == n-1 && newSize%testCS != 0 {
				size = uint32(newSize % testCS)
			}
			c.NewChunks = append(c.NewChunks, ChunkDesc{Hash: hh, Size: size, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}})
			g.hashes = append(g.hashes, hh)
		}
		c.Writes = append(c.Writes, ChunkWrite{uint32(i), hh})
	}
	return Command{Op: OpCommit, Commit: c}
}

func applyAll(t *testing.T, f *FSM, logs []*raft.Log) {
	t.Helper()
	for _, l := range logs {
		f.Apply(l)
	}
}

// Invariant: I-4, I-5
// Lifecycle: L-60, L-159
func TestFSM_ApplySnapshotRestoreEquivalence(t *testing.T) {
	const n = 5000
	rng := testseed.Seed(t)
	ref := NewFSM()
	gen := &cmdGen{rng: rng, f: ref, now: 1000}
	cur := NewFSM() // snapshot/restore path
	var idx uint64
	bootstrap := Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}}
	logs := []*raft.Log{{Index: 1, Type: raft.LogCommand, Data: EncodeCommand(bootstrap)}}
	idx = 1
	ref.Apply(logs[0])
	cur.Apply(logs[0])
	okCount := 0
	sawTomb := false
	for i := range n {
		c := gen.next()
		idx++
		l := &raft.Log{Index: idx, Type: raft.LogCommand, Data: EncodeCommand(c)}
		r1 := ref.Apply(l).(Result)
		r2 := cur.Apply(l).(Result)
		if !errors.Is(r1.Err, r2.Err) && (r1.Err != nil || r2.Err != nil) {
			t.Fatalf("step %d: results differ: %v vs %v", i, r1.Err, r2.Err)
		}
		if r1.Err == nil {
			okCount++
		}
		if len(ref.state().Tombs) > 0 || len(r1.Victims) > 0 {
			sawTomb = true
		}
		if v := CheckInvariants(ref.state()); len(v) != 0 {
			t.Fatalf("step %d op %d: %v", i, c.Op, v)
		}
		if rng.Intn(200) == 0 {
			snap, err := cur.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			sink := &memSink{}
			if err := snap.Persist(sink); err != nil {
				t.Fatal(err)
			}
			snap.Release()
			fresh := NewFSM()
			if err := fresh.Restore(sink.reader()); err != nil {
				t.Fatal(err)
			}
			if fresh.Applied() != cur.Applied() {
				t.Fatalf("restored applied %d != %d", fresh.Applied(), cur.Applied())
			}
			cur = fresh
		}
	}
	if okCount < n/4 {
		t.Fatalf("generator too weak: only %d/%d commands succeeded", okCount, n)
	}
	a, b := encodeState(t, ref.state()), encodeState(t, cur.state())
	if !bytes.Equal(a, b) {
		t.Fatal("replayed and restored states differ")
	}
	if len(ref.state().Chunks) == 0 || !sawTomb {
		t.Fatal("generator never produced tombstones")
	}
}

// Invariant: I-4, I-43
// Lifecycle: L-159
func TestFSM_DeterministicAcrossReplicas(t *testing.T) {
	rng := testseed.Seed(t)
	gen := &cmdGen{rng: rng, f: NewFSM(), now: 5}
	logs := []*raft.Log{{Index: 1, Type: raft.LogCommand, Data: EncodeCommand(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})}}
	gen.f.Apply(logs[0])
	for i := 2; i <= 2000; i++ {
		c := gen.next()
		l := &raft.Log{Index: uint64(i), Type: raft.LogCommand, Data: EncodeCommand(c)}
		gen.f.Apply(l)
		logs = append(logs, l)
	}
	want := encodeState(t, gen.f.state())
	for r := range 3 {
		f := NewFSM()
		applyAll(t, f, logs)
		if got := encodeState(t, f.state()); !bytes.Equal(got, want) {
			t.Fatalf("replica %d diverged", r)
		}
	}
}

func TestFSM_Persist_ConcurrentWithApply(t *testing.T) {
	f := NewFSM()
	f.Apply(&raft.Log{Index: 1, Type: raft.LogCommand, Data: EncodeCommand(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})})
	gen := &cmdGen{rng: testseed.Seed(t), f: f, now: 10}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		idx := uint64(2)
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: EncodeCommand(gen.next())})
			idx++
		}
	})
	for i := range 20 {
		snap, err := f.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		sink := &memSink{}
		if err := snap.Persist(sink); err != nil {
			t.Fatal(err)
		}
		snap.Release()
		if !sink.closed || sink.cancelled {
			t.Fatal("sink not closed cleanly")
		}
		st, err := DecodeState(sink.reader())
		if err != nil {
			t.Fatal(err)
		}
		if v := CheckInvariants(st); len(v) != 0 {
			t.Fatalf("snapshot %d violates invariants: %v", i, v)
		}
	}
	close(stop)
	wg.Wait()
}

func TestFSM_Persist_SinkErrorCancels(t *testing.T) {
	f := NewFSM()
	snap, _ := f.Snapshot()
	s := &failSink{}
	if err := snap.Persist(s); err == nil {
		t.Fatal("expected error")
	}
	if !s.cancelled {
		t.Fatal("sink not cancelled")
	}
}

type failSink struct{ cancelled bool }

func (s *failSink) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (s *failSink) Close() error              { return nil }
func (s *failSink) ID() string                { return "x" }
func (s *failSink) Cancel() error             { s.cancelled = true; return nil }

func TestFSM_Restore_RejectsGarbage(t *testing.T) {
	f := NewFSM()
	f.Apply(&raft.Log{Index: 1, Type: raft.LogCommand, Data: EncodeCommand(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})})
	before := encodeState(t, f.state())
	if err := f.Restore(&readCloser{Reader: bytes.NewReader([]byte("nope"))}); err == nil {
		t.Fatal("garbage restored")
	}
	if !bytes.Equal(before, encodeState(t, f.state())) {
		t.Fatal("failed restore changed state")
	}
}

func TestFSM_WaitApplied(t *testing.T) {
	f := NewFSM()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.WaitApplied(ctx, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.WaitApplied(ctx, 3) }()
	for i := uint64(1); i <= 2; i++ {
		f.Apply(&raft.Log{Index: i, Type: raft.LogNoop})
	}
	select {
	case err := <-done:
		t.Fatalf("returned early: %v", err)
	default:
	}
	f.Apply(&raft.Log{Index: 3, Type: raft.LogNoop})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithCancel(context.Background())
	go func() { done <- f.WaitApplied(cctx, 100) }()
	ccancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	// restore wakes waiters
	go func() { done <- f.WaitApplied(ctx, 50) }()
	g := NewFSM()
	g.Apply(&raft.Log{Index: 60, Type: raft.LogNoop})
	snap, _ := g.Snapshot()
	sink := &memSink{}
	_ = snap.Persist(sink)
	if err := f.Restore(sink.reader()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Read API

func TestStore_ReadAPI(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	f := tf.create(d, "f")
	tf.create(d, "a")
	tf.ok(Command{Op: OpCreate, Create: &Create{Parent: d, Name: "l", Type: TSymlink, Target: "f", Now: 1}})
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS + 100, Writes: []ChunkWrite{{0, a}, {1, HashZero}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})

	if in, ok := tf.f.Lookup(d, "f"); !ok || in.Ino != f {
		t.Fatal("Lookup")
	}
	if _, ok := tf.f.Lookup(f, "x"); ok {
		t.Fatal("Lookup on a file")
	}
	if in, err := tf.f.Resolve([]string{"d", "f"}); err != nil || in.Ino != f {
		t.Fatalf("Resolve %v", err)
	}
	if in, err := tf.f.Resolve(nil); err != nil || in.Ino != RootIno {
		t.Fatalf("Resolve root %v", err)
	}
	if _, err := tf.f.Resolve([]string{"d", "f", "x"}); !errors.Is(err, ErrNotDir) {
		t.Fatalf("Resolve through file: %v", err)
	}
	if _, err := tf.f.Resolve([]string{"zz"}); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Resolve missing: %v", err)
	}
	if p, err := tf.f.PathOf(f); err != nil || len(p) != 2 || p[0] != "d" || p[1] != "f" {
		t.Fatalf("PathOf %v %v", p, err)
	}
	if p, err := tf.f.PathOf(RootIno); err != nil || len(p) != 0 {
		t.Fatalf("PathOf root %v %v", p, err)
	}
	if _, err := tf.f.PathOf(999); !errors.Is(err, ErrNotExist) {
		t.Fatal("PathOf missing")
	}
	ents, err := tf.f.ReadDir(d)
	if err != nil || len(ents) != 3 || ents[0].Name != "a" || ents[1].Name != "f" || ents[2].Name != "l" {
		t.Fatalf("ReadDir %v %v", ents, err)
	}
	if _, err := tf.f.ReadDir(f); !errors.Is(err, ErrNotDir) {
		t.Fatal("ReadDir file")
	}
	if _, err := tf.f.ReadDir(999); !errors.Is(err, ErrNotExist) {
		t.Fatal("ReadDir missing")
	}
	// copies: a returned inode carries no name table at all (docs/02 §Reads)
	if in, _ := tf.f.GetInode(d); in.Children != nil {
		t.Fatal("GetInode returned the Children map")
	}
	c, _ := tf.f.GetChunk(a)
	c.Placement[0] = 99
	if c2, _ := tf.f.GetChunk(a); c2.Placement[0] == 99 {
		t.Fatal("GetChunk returned shared placement")
	}
	var seen int
	tf.f.ForEachChunk(func(ch Chunk) bool { seen++; return true })
	if seen != 1 {
		t.Fatalf("ForEachChunk saw %d", seen)
	}
	tf.f.ForEachChunk(func(ch Chunk) bool { seen++; return false })
	st := tf.f.Stats()
	if st.Inodes != 5 || st.Chunks != 1 || st.LogicalBytes != testCS+100 || st.StoredBytes != testCS/2*3 || st.Tombstoned != 0 {
		t.Fatalf("stats %+v", st)
	}
	if ns := tf.f.Nodes(); len(ns) != 0 {
		t.Fatal("nodes")
	}
}

// ---------------------------------------------------------------------------
// Invariant checker

// Invariant: I-27
// Lifecycle: L-150, L-151
func TestCheckInvariants_DetectsEachViolation(t *testing.T) {
	build := func() *State {
		tf := newTFSM(t)
		d := tf.mkdir(RootIno, "d")
		f := tf.create(d, "f")
		g := tf.create(d, "g")
		a, b := sha("a"), sha("b")
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: 2 * testCS, Writes: []ChunkWrite{{0, a}, {1, b}}, NewChunks: []ChunkDesc{desc(a), desc(b)}, Now: 1}})
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: g, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, Now: 2}})
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: g, BaseVersion: 1, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 3}})           // nothing tombstoned (a still ref'd by f)
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: testCS, NewSize: testCS, Now: 4}}) // b tombstoned
		return tf.f.state().Clone()
	}
	if v := CheckInvariants(build()); len(v) != 0 {
		t.Fatalf("clean state reported %v", v)
	}
	cases := []struct {
		name string
		mut  func(s *State)
		want string
	}{
		{"refs too high", func(s *State) { s.Chunks[sha("a")].Refs++ }, "refs"},
		{"refs too low", func(s *State) { s.Chunks[sha("a")].Refs-- }, "refs"},
		{"chunk list length", func(s *State) { s.Inodes[findIno(s, "f")].Size += testCS }, "chunk count"},
		{"missing parent entry", func(s *State) { delete(s.Inodes[findIno(s, "d")].Children, "f") }, "parent"},
		{"wrong parent", func(s *State) { s.Inodes[findIno(s, "f")].Parent = 999 }, "parent"},
		{"cycle", func(s *State) {
			d := s.Inodes[findIno(s, "d")]
			d.Parent = d.Ino
			d.Children["d"] = d.Ino
			delete(s.Inodes[RootIno].Children, "d")
		}, "root"},
		{"tombs missing", func(s *State) { delete(s.Tombs, sha("b")) }, "tombs"},
		{"tombs extra", func(s *State) { s.Tombs[sha("a")] = struct{}{} }, "tombs"},
		{"hashzero entry", func(s *State) { s.Chunks[HashZero] = &Chunk{} }, "HashZero"},
		{"dir nlink", func(s *State) { s.Inodes[RootIno].Nlink = 7 }, "nlink"},
		{"nextino", func(s *State) { s.NextIno = 2 }, "NextIno"},
		{"tombstone time", func(s *State) { s.Chunks[sha("b")].TombstonedAt = 0 }, "tombstone"},
		{"live chunk with tombstone time", func(s *State) { s.Chunks[sha("a")].TombstonedAt = 5 }, "tombstone"},
		{"dangling child", func(s *State) { s.Inodes[RootIno].Children["ghost"] = 4242 }, "child"},
		{"child name mismatch", func(s *State) { s.Inodes[findIno(s, "f")].Name = "zz" }, "parent"},
		{"placement length", func(s *State) { s.Chunks[sha("a")].Placement = []NodeIdx{1} }, "placement"},
		{"unreferenced chunk missing from table", func(s *State) { delete(s.Chunks, sha("a")) }, "unknown chunk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := build()
			tc.mut(s)
			v := CheckInvariants(s)
			if len(v) == 0 {
				t.Fatalf("violation not detected")
			}
			found := false
			for _, m := range v {
				if containsFold(m, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("messages %v do not mention %q", v, tc.want)
			}
		})
	}
}

func findIno(s *State, name string) Ino {
	for ino, in := range s.Inodes {
		if in.Name == name {
			return ino
		}
	}
	panic("no inode " + name)
}

func containsFold(s, sub string) bool {
	return bytes.Contains(bytes.ToLower([]byte(s)), bytes.ToLower([]byte(sub)))
}

// A node that shuts down with a proposal in flight must not leave the proposer waiting
// forever: WaitApplied returns ErrClosed (the Windows CI run of TestCrash_AtEveryPoint
// hung here for ten minutes).
// Invariant: I-29
func TestFSM_WaitApplied_ReturnsOnClose(t *testing.T) {
	f := NewFSM()
	done := make(chan error, 1)
	go func() { done <- f.WaitApplied(context.Background(), 5) }()
	f.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitApplied did not return after Close")
	}
	if err := f.WaitApplied(context.Background(), 5); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
	if err := f.WaitApplied(context.Background(), 0); err != nil {
		t.Fatalf("already applied index must still succeed: %v", err)
	}
}

// A join retried after a timeout may reach a leader whose FSM copy has not applied the
// first upsert; the FSM itself must keep one record per node ID.
// Invariant: I-62
func TestFSM_NodeUpsert_IdempotentByID(t *testing.T) {
	f := NewFSM()
	apply := func(idx uint64, c Command) Result {
		return f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: EncodeCommand(c)}).(Result)
	}
	apply(1, Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: 65536, K: 2, M: 1, Now: 1}})
	nd := Node{ID: "aaaaaaaaaaaaaaaa", Name: "a", Kind: KindDevice, Addr: "10.0.0.1", RaftPort: 1, RPCPort: 2, Capacity: 10}
	r1 := apply(2, Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: nd}})
	nd.Capacity = 20 // the retry carries fresher fields
	r2 := apply(3, Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: nd}})
	if r1.Err != nil || r2.Err != nil || r1.Ino != r2.Ino {
		t.Fatalf("retry allocated a new index: %+v %+v", r1, r2)
	}
	nodes := f.Nodes()
	if len(nodes) != 1 || nodes[0].Capacity != 20 || nodes[0].State != Active {
		t.Fatalf("nodes after retry: %+v", nodes)
	}
	// a different ID still gets its own index
	other := nd
	other.ID = "bbbbbbbbbbbbbbbb"
	r3 := apply(4, Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: other}})
	if r3.Err != nil || r3.Ino == r1.Ino {
		t.Fatalf("second node: %+v", r3)
	}
	// a removed record is not revived by a bare upsert
	apply(5, Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: NodeIdx(r3.Ino), State: Removed}})
	if r4 := apply(6, Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: other}}); r4.Err != nil || r4.Ino == r3.Ino {
		t.Fatalf("upsert after removal must allocate anew: %+v", r4)
	}
}
