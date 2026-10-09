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
	Ino      Ino
	Gen      uint32
	Type     FileType
	Mode     uint32
	Uid, Gid uint32
	Size     uint64
	Atime    int64
	Mtime    int64
	Ctime    int64
	Nlink    uint16
	Version  uint64
	Parent   Ino
	Name     string
	Chunks   []Hash         // TFile only; len == ceil(Size/ChunkSize)
	Target   string         // TSymlink only
	Children map[string]Ino // TDir only
}

// Chunk is one entry of the content-addressed chunk table.
type Chunk struct {
	Hash         Hash
	Size         uint32
	K, M         uint8
	Refs         uint32
	Placement    []NodeIdx // len K+M; Placement[i] holds shard i; 0 = missing
	TombstonedAt int64     // unix nanos when Refs reached 0; 0 while live
}

// NodeKind distinguishes devices (Raft members, gateways, storage) from S3 buckets
// (storage only).
type NodeKind uint8

// Node kinds.
const (
	KindDevice NodeKind = 1
	KindS3     NodeKind = 2
	KindDisk   NodeKind = 3 // an added disk of a device: a placement target, never a Raft member (docs/14)
)

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
	Host       NodeIdx // KindDisk: the device that serves this disk; 0 for every other kind
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
	for _, k := range []NodeKind{KindDevice, KindDisk, KindS3} {
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
	Applied  uint64
	LastNow  int64 // monotonic FSM clock: max of every applied command's Now
}

// NewState returns an empty state containing only the root directory.
func NewState() *State {
	return &State{
		NextIno:  RootIno + 1,
		NextNode: 1,
		Inodes: map[Ino]*Inode{
			RootIno: {Ino: RootIno, Type: TDir, Mode: 0o755, Nlink: 2, Children: map[string]Ino{}},
		},
		Chunks: map[Hash]*Chunk{},
		Nodes:  map[NodeIdx]*Node{},
		Tombs:  map[Hash]struct{}{},
	}
}

// Clone returns a deep copy of s suitable for a snapshot taken while Apply keeps running
// on the original. Inode.Chunks slices are shared because they are immutable.
func (s *State) Clone() *State {
	c := &State{
		Vol:      s.Vol,
		Meta:     s.Meta,
		NextIno:  s.NextIno,
		NextNode: s.NextNode,
		Applied:  s.Applied,
		LastNow:  s.LastNow,
		Inodes:   make(map[Ino]*Inode, len(s.Inodes)),
		Chunks:   make(map[Hash]*Chunk, len(s.Chunks)),
		Nodes:    make(map[NodeIdx]*Node, len(s.Nodes)),
		Tombs:    make(map[Hash]struct{}, len(s.Tombs)),
	}
	for ino, in := range s.Inodes {
		cp := *in
		if in.Children != nil {
			cp.Children = make(map[string]Ino, len(in.Children))
			maps.Copy(cp.Children, in.Children)
		}
		c.Inodes[ino] = &cp
	}
	for h, ch := range s.Chunks {
		cp := *ch
		cp.Placement = append([]NodeIdx(nil), ch.Placement...)
		c.Chunks[h] = &cp
	}
	for idx, n := range s.Nodes {
		cp := *n
		c.Nodes[idx] = &cp
	}
	for h := range s.Tombs {
		c.Tombs[h] = struct{}{}
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

// NewJoinToken returns a fresh join token: 8 random base32 characters (docs/08 §Join).
func NewJoinToken() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("meta: crypto/rand: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}
