// Package meta holds the replicated metadata model of a tailraid volume: the types that
// live in the Raft-replicated State, the command set applied to it, the canonical binary
// codec for log entries and snapshots, and the placement function.
//
// See docs/01-data-model.md, docs/02-consistency-and-raft.md and
// docs/03-erasure-coding-and-placement.md.
package meta

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Hash is the SHA-256 of the logical bytes of a chunk.
type Hash [32]byte

// HashZero is the sentinel for an all-zero chunk. It is never stored, uploaded or
// ref-counted (I-2).
var HashZero Hash

// NodeIdx is a dense index into State.Nodes. 0 means "no node" (a missing placement slot).
type NodeIdx uint16

// Ino identifies an inode. Values are allocated from State.NextIno and never reused (I-3).
type Ino uint64

// RootIno is the inode number of the volume root directory.
const RootIno Ino = 1

// FileType is the kind of an inode.
type FileType uint8

// Inode types.
const (
	TFile    FileType = 1
	TDir     FileType = 2
	TSymlink FileType = 3
)

// Inode is one file, directory or symlink. Chunks is treated as immutable: every Commit
// builds a new slice, so a copied Inode always sees a consistent chunk list.
type Inode struct {
	Ino       Ino
	Gen       uint32
	Type      FileType
	Mode      uint32
	Uid, Gid  uint32
	Size      uint64
	Atime     int64
	Mtime     int64
	Ctime     int64
	Nlink     uint16
	Version   uint64
	Parent    Ino
	Name      string
	Flags     uint8          // FlagProtected, FlagNSFW, the library kind in FlagLibraryMask (docs/02 §Apply rules, docs/18 §9); directories only
	DeletedAt int64          // FSM clock of the delete that made this entry a trash root; 0 while live
	RewriteAt uint64         // TFile: the Version the current rewrite started at (docs/04 §6, I-223); 0 for none
	Chunks    []Hash         // TFile only; len == ceil(Size/ChunkSize)
	Target    string         // TSymlink only
	Children  map[string]Ino // TDir only
}

// Directory flags (docs/01). FlagProtected is enforced by the FSM (I-154); FlagNSFW is
// carried for the browser UI; the bits under FlagLibraryMask hold the folder's library
// kind (docs/18 §9): two bits at codec 6 (I-210), three since codec 10, whose kind 4 is
// roms (I-277).
const (
	FlagProtected uint8 = 1 << 0
	FlagNSFW      uint8 = 1 << 1

	FlagLibraryShift       = 2
	FlagLibraryMask  uint8 = 0b111 << FlagLibraryShift

	knownFlags = FlagProtected | FlagNSFW | FlagLibraryMask
)

// LibraryKind is what the Media tab lists a flagged folder as (docs/18 §9).
type LibraryKind uint8

const (
	LibraryNone LibraryKind = iota
	LibraryMovies
	LibraryTV
	LibraryMusic
	LibraryRoms // codec 10 (I-277)
)

var libraryWords = [...]string{"none", "movies", "tv", "music", "roms"}

// Known reports whether k is a kind this binary knows: kinds 5 to 7 are malformed.
func (k LibraryKind) Known() bool { return int(k) < len(libraryWords) }

func (k LibraryKind) String() string {
	if int(k) < len(libraryWords) {
		return libraryWords[k]
	}
	return "none"
}

// ParseLibraryKind reads the word of a kind: exactly one of none, movies, tv, music, roms.
func ParseLibraryKind(s string) (LibraryKind, bool) {
	for i, w := range libraryWords {
		if s == w {
			return LibraryKind(i), true
		}
	}
	return LibraryNone, false
}

// Library is the kind in in's own flags (none unless the directory was flagged).
func (in Inode) Library() LibraryKind {
	return LibraryKind(in.Flags&FlagLibraryMask) >> FlagLibraryShift
}

// WithLibrary returns flags with the library bits set to k.
func WithLibrary(flags uint8, k LibraryKind) uint8 {
	return flags&^FlagLibraryMask | uint8(k)<<FlagLibraryShift&FlagLibraryMask
}

// UploadSuffix ends a web upload's temp name, ".<name>.tailraid-upload" (docs/13 §4).
const UploadSuffix = ".tailraid-upload"

// ReservedDir is the root folder that holds the browser UI's thumbnails (docs/13 §5);
// everything under it is ephemeral.
const ReservedDir = ".tailraid"

// IsUploadTemp reports whether name is a web upload's temp (docs/13 §4, I-81).
func IsUploadTemp(name string) bool {
	return len(name) > len(UploadSuffix)+1 && strings.HasPrefix(name, ".") && strings.HasSuffix(name, UploadSuffix)
}

// IsOSMetadata reports whether name is a file macOS writes for its own use beside the
// user's: "._<name>" (AppleDouble) and ".DS_Store" (Finder's view settings) (I-140).
func IsOSMetadata(name string) bool {
	return (len(name) > 2 && strings.HasPrefix(name, "._")) || name == ".DS_Store"
}

// IsEphemeral is the one list of names a delete unlinks rather than trashes (docs/06 §1,
// I-157): an upload's temp and the macOS metadata files. The thumbnails under
// ReservedDir are the other ephemeral entries; the FSM knows them by their place.
func IsEphemeral(name string) bool { return IsUploadTemp(name) || IsOSMetadata(name) }

// Chunk is one entry of the content-addressed chunk table.
type Chunk struct {
	Hash         Hash
	Size         uint32
	K, M         uint8
	Refs         uint32
	Placement    []NodeIdx // len K+M; Placement[i] holds shard i; 0 = missing
	TombstonedAt int64     // unix nanos when Refs reached 0; 0 while live
	// Sums is the SHA-256 of each data shard's stored payload (len K) once recorded by
	// the flusher or the backfill (docs/04 §4, I-201); nil until then. Never modified in
	// place: copy on write like Placement.
	Sums []Hash
	// SumsKind says what Sums are: SumsShard, the SHA-256 of each data shard's payload,
	// or SumsBlocks, the root of each data shard's block table (docs/04 §5, I-422).
	// Zero without Sums.
	SumsKind uint8
}

// The kinds of Chunk.Sums (docs/04 §5, I-422).
const (
	SumsShard  uint8 = 0 // the whole-shard SHA-256 (codec 5)
	SumsBlocks uint8 = 1 // the root of the chunk.BlockSize block table (codec 18)
)

// NodeKind distinguishes devices (Raft members, gateways, storage) from S3 buckets
// (storage only).
type NodeKind uint8

// Node kinds.
const (
	KindDevice NodeKind = 1
	KindS3     NodeKind = 2
	KindDisk   NodeKind = 3 // an added disk of a device: a placement target, never a Raft member (docs/14)
	KindMirror NodeKind = 4 // a disk of a device given to snapshots: never a placement target (docs/16)
)

// Target reports whether a node of this kind can hold a chunk's shard: anything but a
// device, which stores nothing (docs/14 §9), and a mirror, which holds snapshots, not
// slots (docs/16 §1). The filters that use it said "not a device" before mirrors existed.
func (k NodeKind) Target() bool { return k != KindDevice && k != KindMirror }

// NodeState is the membership state of a node.
type NodeState uint8

// Node states. Removed is terminal.
const (
	Active   NodeState = 1
	Draining NodeState = 2
	Removed  NodeState = 3
)

// Node is one member of the node table. Idx values are never reused so historical
// placements stay interpretable. Credentials are never stored here.
type Node struct {
	Idx        NodeIdx
	ID         string // cluster node ID: 16 random hex chars; raft.ServerID for devices
	Name       string
	Kind       NodeKind
	State      NodeState
	TSStableID string // Tailscale stable node ID; authorization only
	Addr       string // canonical Tailscale IP text; empty for S3 nodes
	RaftPort   uint16
	RPCPort    uint16
	Voter      bool   // devices only
	Capacity   uint64 // shard-byte budget; also the placement weight
	Version    string // software version last reported
	JoinedAt   int64
	Host       NodeIdx // KindDisk, KindMirror: the device that serves the disk; 0 for every other kind
	// S3 nodes only
	S3Endpoint  string
	S3Bucket    string
	S3Prefix    string
	S3Region    string
	S3PathStyle bool
}

// String names a node state as `status` and the reports print it.
func (s NodeState) String() string {
	switch s {
	case Draining:
		return "draining"
	case Removed:
		return "removed"
	case Active:
		return "active"
	}
	return "active" // the zero value, written by nodes that predate the field
}

// ParseNodeState is the inverse of NodeState.String; an unknown name is an error.
func ParseNodeState(s string) (NodeState, error) {
	for _, st := range []NodeState{Active, Draining, Removed} {
		if st.String() == s {
			return st, nil
		}
	}
	return 0, fmt.Errorf("meta: unknown node state %q", s)
}

// ParseNodeKind is the inverse of NodeKind.String; an unknown name is an error.
func ParseNodeKind(s string) (NodeKind, error) {
	for _, k := range []NodeKind{KindDevice, KindDisk, KindS3, KindMirror} {
		if k.String() == s {
			return k, nil
		}
	}
	return 0, fmt.Errorf("meta: unknown node kind %q", s)
}

// String names a node kind.
func (k NodeKind) String() string {
	switch k {
	case KindS3:
		return "s3"
	case KindDisk:
		return "disk"
	case KindMirror:
		return "mirror"
	case KindDevice:
		return "device"
	}
	return "device" // the zero value, written by nodes that predate the field
}

// HexID is the printed form of a cluster ID.
func HexID(id [16]byte) string { return hex.EncodeToString(id[:]) }

// SplitPath cleans a slash- or backslash-separated path into components relative to the
// root: "" or "/" is the root (nil).
func SplitPath(name string) []string {
	name = path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	if name == "/" {
		return nil
	}
	return strings.Split(name[1:], "/")
}

// LiveSet is a set view of a liveness list.
func LiveSet(live []NodeIdx) map[NodeIdx]bool {
	out := make(map[NodeIdx]bool, len(live))
	for _, n := range live {
		out[n] = true
	}
	return out
}

// RPCAddr is the node's host:port for member RPC; RaftAddr the Raft stream's.
func (n Node) RPCAddr() string  { return net.JoinHostPort(n.Addr, strconv.Itoa(int(n.RPCPort))) }
func (n Node) RaftAddr() string { return net.JoinHostPort(n.Addr, strconv.Itoa(int(n.RaftPort))) }

// Volume is the policy for new chunks. ChunkSize is fixed for the life of the volume;
// K and M may change at any time (PolicyVersion bumps).
type Volume struct {
	ChunkSize     uint32
	K, M          uint8
	PolicyVersion uint32
	Auto          bool // the leader picks K, M from the member table (docs/03); set by init
}

// Meta is the cluster identity.
type Meta struct {
	ClusterID [16]byte
	JoinToken string
	CreatedAt int64
}

// State is everything the cluster agrees on. It is only ever mutated by the FSM.
type State struct {
	Vol      Volume
	Meta     Meta
	NextIno  Ino
	NextNode NodeIdx
	Inodes   map[Ino]*Inode
	Chunks   map[Hash]*Chunk
	Nodes    map[NodeIdx]*Node
	Tombs    map[Hash]struct{} // index of Chunks with Refs == 0
	// Trash indexes the deleted roots (inodes with DeletedAt != 0, absent from their
	// parent's Children) by the parent they came from (docs/01). Derived like Tombs:
	// never serialized, rebuilt on decode and in Clone.
	Trash map[Ino]map[Ino]struct{}
	// Snaps is the snapshot catalogue (docs/16 §5): the snapshots themselves live on
	// their mirrors; a row's only effect on the pool is the sync hold of GCDelete.
	Snaps    map[SnapID]*Snapshot
	NextSnap SnapID
	// Secrets is the secrets store (docs/19 §4): sealed values by name, and the ID of
	// the cluster key they are sealed under (zero: no key yet).
	Secrets     map[string]*Secret
	SecretKeyID KeyID
	// Users, groups and permissions (docs/21, codec 14): the identity rows, the grant
	// table keyed by inode and subject, the open invites and the browser sessions.
	Users     map[UserIdx]*User
	NextUser  UserIdx
	Groups    map[GroupIdx]*Group
	NextGroup GroupIdx
	Grants    map[GrantKey]*Grant
	Invites   map[[32]byte]*Invite
	Sessions  map[[32]byte]*Session
	// Playback holds each viewer's rows by file (docs/13 §10c, codec 17).
	Playback map[string]map[Ino]*PlayRow
	Applied  uint64
	LastNow  int64 // monotonic FSM clock: max of every applied command's Now
}

// SnapID identifies a snapshot; allocated from State.NextSnap, never reused, never 0.
type SnapID uint32

// SnapState is where a snapshot is in its life (docs/16 §3, §5).
type SnapState uint8

// Snapshot states.
const (
	SnapSyncing    SnapState = 1 // being copied to its mirror; holds GC (I-181)
	SnapComplete   SnapState = 2
	SnapIncomplete SnapState = 3 // complete but for Missing chunks the sync could not read
	SnapDeleting   SnapState = 4 // chosen by thinning; its mirror's host has not swept it yet
)

// String names a snapshot state as the reports print it.
func (s SnapState) String() string {
	switch s {
	case SnapSyncing:
		return "syncing"
	case SnapComplete:
		return "complete"
	case SnapIncomplete:
		return "incomplete"
	case SnapDeleting:
		return "deleting"
	}
	return fmt.Sprintf("SnapState(%d)", uint8(s))
}

// Snapshot is one catalogue row: the volume as of every entry up to Index, held on
// Mirror (docs/16).
type Snapshot struct {
	ID       SnapID
	Mirror   NodeIdx // a KindMirror row
	Name     string  // unique per mirror
	Index    uint64  // the SnapshotBegin's own Raft index
	At       int64   // FSM clock at the begin; the sync hold's bound
	Auto     bool    // taken by the leader's schedule (thinned); false = manual, permanent
	State    SnapState
	Root     Hash // the root tree on the mirror, once ended
	Files    uint64
	Bytes    uint64
	NewBytes uint64
	Missing  uint32
}

// MaxSnapName bounds a snapshot's name (docs/16 §8).
const MaxSnapName = 64

// validSnapName reports whether name may name a snapshot: 1..MaxSnapName bytes, no '/'
// or NUL, not "." or "..".
func validSnapName(name string) bool {
	return name != "" && len(name) <= MaxSnapName && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// syncHold is the GC bound the syncing rows impose (I-181): the oldest syncing row's At,
// or ok false when nothing is syncing.
func (s *State) syncHold() (at int64, ok bool) {
	for _, sn := range s.Snaps {
		if sn.State == SnapSyncing && (!ok || sn.At < at) {
			at, ok = sn.At, true
		}
	}
	return at, ok
}

// NewState returns an empty state containing only the root directory.
func NewState() *State {
	return &State{
		NextIno:  RootIno + 1,
		NextNode: 1,
		Inodes: map[Ino]*Inode{
			RootIno: {Ino: RootIno, Type: TDir, Mode: 0o755, Nlink: 2, Children: map[string]Ino{}},
		},
		Chunks:   map[Hash]*Chunk{},
		Nodes:    map[NodeIdx]*Node{},
		Tombs:    map[Hash]struct{}{},
		Trash:    map[Ino]map[Ino]struct{}{},
		Snaps:    map[SnapID]*Snapshot{},
		NextSnap: 1,
		Secrets:  map[string]*Secret{},
		Users:    map[UserIdx]*User{},
		Groups:   map[GroupIdx]*Group{},
		Grants:   map[GrantKey]*Grant{},
		Invites:  map[[32]byte]*Invite{},
		Sessions: map[[32]byte]*Session{},
		Playback: map[string]map[Ino]*PlayRow{},
	}
}

// indexTrash records ino as a deleted root under parent.
func (s *State) indexTrash(parent, ino Ino) {
	set := s.Trash[parent]
	if set == nil {
		set = map[Ino]struct{}{}
		s.Trash[parent] = set
	}
	set[ino] = struct{}{}
}

// unindexTrash forgets the root ino under parent.
func (s *State) unindexTrash(parent, ino Ino) {
	if set := s.Trash[parent]; set != nil {
		delete(set, ino)
		if len(set) == 0 {
			delete(s.Trash, parent)
		}
	}
}

// lineage is what the parent chain of an inode says about it (docs/02 §Protection):
// whether it lies in the trash (it or an ancestor is a deleted root), under a protected
// directory (itself included), or under the reserved folder of docs/13 §5.
type lineage struct {
	inTrash   bool
	protected bool
	reserved  bool
}

// lineage walks from ino to the root: one map lookup per level, what pathLen pays.
func (s *State) lineage(ino Ino) lineage {
	var l lineage
	for cur, steps := ino, 0; steps <= len(s.Inodes); steps++ {
		in, ok := s.Inodes[cur]
		if !ok {
			return l
		}
		if in.DeletedAt != 0 {
			l.inTrash = true
		}
		if in.Type == TDir && in.Flags&FlagProtected != 0 {
			l.protected = true
		}
		if in.Parent == RootIno && in.Name == ReservedDir {
			l.reserved = true
		}
		if cur == RootIno || in.Parent == 0 {
			return l
		}
		cur = in.Parent
	}
	return l
}

// mutChunk is the entry of chunk h for an apply to change: a fresh copy installed under
// the hash, so a clone sharing the old entry never sees the change (I-383). Every apply
// that changes a chunk goes through it; the slices are replaced, never written into.
func (s *State) mutChunk(h Hash) *Chunk {
	cp := *s.Chunks[h]
	s.Chunks[h] = &cp
	return &cp
}

// Clone returns a copy of s suitable for a snapshot taken while Apply keeps running on
// the original. Inode.Chunks slices are shared because they are immutable, and so are the
// chunk entries, which an apply replaces through mutChunk instead of changing (I-383).
// A clone is read-only: an append on the original extends a chunk list in capacity the
// clone's copy of that list shares (I-385), so applying to both would collide.
func (s *State) Clone() *State {
	c := &State{
		Vol:         s.Vol,
		Meta:        s.Meta,
		NextIno:     s.NextIno,
		NextNode:    s.NextNode,
		Applied:     s.Applied,
		LastNow:     s.LastNow,
		Inodes:      make(map[Ino]*Inode, len(s.Inodes)),
		Chunks:      maps.Clone(s.Chunks), // the entries are shared: never changed in place (mutChunk, I-383)
		Nodes:       make(map[NodeIdx]*Node, len(s.Nodes)),
		Tombs:       maps.Clone(s.Tombs),
		Trash:       make(map[Ino]map[Ino]struct{}, len(s.Trash)),
		Snaps:       make(map[SnapID]*Snapshot, len(s.Snaps)),
		NextSnap:    s.NextSnap,
		Secrets:     make(map[string]*Secret, len(s.Secrets)),
		SecretKeyID: s.SecretKeyID,
		Users:       make(map[UserIdx]*User, len(s.Users)),
		NextUser:    s.NextUser,
		Groups:      make(map[GroupIdx]*Group, len(s.Groups)),
		NextGroup:   s.NextGroup,
		Grants:      make(map[GrantKey]*Grant, len(s.Grants)),
		Invites:     make(map[[32]byte]*Invite, len(s.Invites)),
		Sessions:    make(map[[32]byte]*Session, len(s.Sessions)),
		Playback:    clonePlayback(s.Playback),
	}
	for idx, u := range s.Users {
		cp := *u
		cp.Keys = slices.Clone(u.Keys)
		c.Users[idx] = &cp
	}
	for idx, g := range s.Groups {
		cp := *g
		cp.Members = slices.Clone(g.Members)
		c.Groups[idx] = &cp
	}
	for k, g := range s.Grants {
		cp := *g
		c.Grants[k] = &cp
	}
	for h, inv := range s.Invites {
		cp := *inv
		c.Invites[h] = &cp
	}
	for h, se := range s.Sessions {
		cp := *se
		c.Sessions[h] = &cp
	}
	for n, sec := range s.Secrets {
		cp := *sec
		cp.Cipher = slices.Clone(sec.Cipher)
		c.Secrets[n] = &cp
	}
	for id, sn := range s.Snaps {
		cp := *sn
		c.Snaps[id] = &cp
	}
	for parent, set := range s.Trash {
		cp := make(map[Ino]struct{}, len(set))
		maps.Copy(cp, set)
		c.Trash[parent] = cp
	}
	for ino, in := range s.Inodes {
		cp := *in
		cp.Chunks = slices.Clip(in.Chunks) // shared, capped: see copyInode (I-385)
		if in.Children != nil {
			cp.Children = make(map[string]Ino, len(in.Children))
			maps.Copy(cp.Children, in.Children)
		}
		c.Inodes[ino] = &cp
	}
	for idx, n := range s.Nodes {
		cp := *n
		c.Nodes[idx] = &cp
	}
	return c
}

// MaxChunksPerFile bounds a file's chunk list (I-64): the FSM holds every chunk hash of
// every file in memory and a Commit rebuilds the whole list, so no command may ask for
// more. With 1 MiB chunks this is a 1 TiB file (a 32 MiB list on every node).
const MaxChunksPerFile = 1 << 20

// MaxFileSize is the largest size a file may have under chunkSize.
func MaxFileSize(chunkSize uint32) uint64 { return uint64(chunkSize) * MaxChunksPerFile }

// ChunkCount returns ceil(size/chunkSize), the number of chunk slots a file of the given
// size occupies (I-1). It never overflows: the result saturates at math.MaxInt.
func ChunkCount(size uint64, chunkSize uint32) int {
	cs := uint64(chunkSize)
	q := size / cs
	if size%cs != 0 {
		q++
	}
	if q > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(q)
}

// HandleMagic is the first four bytes of every NFS file handle ("TRHD").
const HandleMagic uint32 = 0x54524844

// HandleLen is the fixed size of a file handle.
const HandleLen = 16

// EncodeHandle builds the 16-byte big-endian NFS handle magic | gen | ino (I-16).
func EncodeHandle(ino Ino, gen uint32) []byte {
	b := make([]byte, HandleLen)
	binary.BigEndian.PutUint32(b[0:4], HandleMagic)
	binary.BigEndian.PutUint32(b[4:8], gen)
	binary.BigEndian.PutUint64(b[8:16], uint64(ino))
	return b
}

// DecodeHandle parses a handle produced by EncodeHandle. ok is false for any other input.
func DecodeHandle(b []byte) (ino Ino, gen uint32, ok bool) {
	if len(b) != HandleLen || binary.BigEndian.Uint32(b[0:4]) != HandleMagic {
		return 0, 0, false
	}
	return Ino(binary.BigEndian.Uint64(b[8:16])), binary.BigEndian.Uint32(b[4:8]), true
}

// SnapHandleMagic opens a handle under /.snapshots ("TRHS"), SnapHandleLen its size
// (docs/01 §NFS file handle).
const (
	SnapHandleMagic uint32 = 0x54524853
	SnapHandleLen   int    = 24
)

// SnapGenMirror is the Gen of a mirror's directory handle, whose Ino is the mirror's row.
const SnapGenMirror uint32 = 1<<32 - 1

// SnapHandle names an entry under /.snapshots (I-185): Snap 0 is a synthetic directory
// (Ino 0 /.snapshots itself, Gen SnapGenMirror a mirror's), otherwise the entry of
// snapshot Snap whose tree recorded (Ino, Gen); Ino 0 is the snapshot's root.
type SnapHandle struct {
	Snap SnapID
	Gen  uint32
	Ino  uint64
}

// EncodeSnapHandle builds the 24-byte handle magic | snap | gen | ino | zero.
func EncodeSnapHandle(h SnapHandle) []byte {
	b := make([]byte, SnapHandleLen)
	binary.BigEndian.PutUint32(b[0:4], SnapHandleMagic)
	binary.BigEndian.PutUint32(b[4:8], uint32(h.Snap))
	binary.BigEndian.PutUint32(b[8:12], h.Gen)
	binary.BigEndian.PutUint64(b[12:20], h.Ino)
	return b
}

// DecodeSnapHandle parses a handle produced by EncodeSnapHandle; ok is false otherwise.
func DecodeSnapHandle(b []byte) (SnapHandle, bool) {
	if len(b) != SnapHandleLen || binary.BigEndian.Uint32(b[0:4]) != SnapHandleMagic || binary.BigEndian.Uint32(b[20:24]) != 0 {
		return SnapHandle{}, false
	}
	return SnapHandle{Snap: SnapID(binary.BigEndian.Uint32(b[4:8])), Gen: binary.BigEndian.Uint32(b[8:12]), Ino: binary.BigEndian.Uint64(b[12:20])}, true
}

// NewJoinToken returns a fresh join token: 8 random base32 characters (docs/08 §Join).
func NewJoinToken() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("meta: crypto/rand: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}
