package meta

import "fmt"

// Op identifies a command. Numbers are part of the wire format and are never reused.
type Op uint8

// Commands, in wire order. See docs/02-consistency-and-raft.md for the apply rules.
const (
	OpSetVolume     Op = 1  // {ChunkSize (only at bootstrap), K, M, Now}
	OpMetaSet       Op = 2  // {ClusterID, JoinToken, CreatedAt}
	OpNodeUpsert    Op = 3  // {Node; Idx 0 = allocate}
	OpNodeSetState  Op = 4  // {Idx, State, Now}
	OpCreate        Op = 5  // {Parent, Name, Type, Mode, Uid, Gid, Target, Now}
	OpMkdir         Op = 6  // {Parent, Name, Mode, Uid, Gid, Now}
	OpUnlink        Op = 7  // {Parent, Name, Now}
	OpRmdir         Op = 8  // {Parent, Name, Now}
	OpRename        Op = 9  // {OldParent, OldName, NewParent, NewName, Now}
	OpSetAttr       Op = 10 // {Ino, Mask, Mode, Uid, Gid, Atime, Mtime, Now}
	OpCommit        Op = 11 // {Ino, BaseVersion, HasTruncate, TruncateTo, NewSize, Writes, NewChunks, Now}
	OpSetPlacement  Op = 12 // {Hash, Placement, K, M}
	OpGCDelete      Op = 13 // {Before} → Result.Victims
	OpSetVolumeMode Op = 14 // {Auto, Now}: the leader picks K, M from the member table (docs/03)
	OpTrash         Op = 15 // {Parent, Name, Recursive, Now}: the entry into the trash (codec 4)
	OpRestore       Op = 16 // {Ino, NewParent (0 = original), NewName ("" = original), Now} (codec 4)
	OpPurge         Op = 17 // {Ino (0 = every root due), Before, Limit, Now} (codec 4)
	OpSetShardSums  Op = 18 // {Hash, Sums}: the data shards' hashes of a chunk committed without them (docs/04 §4, codec 5)
	// Snapshots on mirror disks (docs/16, codec 7).
	OpSnapshotBegin  Op = 19 // {Mirror, Name, Auto, Now} → Result.SnapID: a sync begins and holds GC
	OpSnapshotEnd    Op = 20 // {ID, OK, Root, Files, Bytes, NewBytes, Missing, Now}
	OpSnapshotDelete Op = 21 // {ID, Done, Now}: the leader's thinning only, never a user (I-183)
	OpSnapshotImport Op = 22 // {Mirror, After, Name, At, Auto, Root, Files, Bytes, NewBytes, Missing, Now} → Result.SnapID: a rowless manifest (docs/16 §5)
)

// commandVersion is the codec version of a log entry. Version 2 is written only for a
// NodeUpsert that carries a disk (its Node has Host), so a cluster without disks writes
// exactly what an older node reads (docs/12 §6); every version is read.
const (
	commandVersion   = 1
	commandVersionV2 = 2
	commandVersionV3 = 3 // SetPlacement.Expect and GCDelete.Limit
	commandVersionV4 = 4 // Trash, Restore, Purge and a SetAttr carrying Flags (docs/02)
	commandVersionV5 = 5 // a Commit whose NewChunks carry Sums, and SetShardSums (docs/04 §4, I-201)
	commandVersionV6 = 6 // a SetAttr whose Flags carry the library kind (docs/18 §9, I-210)
	commandVersionV7 = 7 // the snapshot commands and a NodeUpsert of a mirror (docs/16, I-184)
	commandVersionV8 = 8 // a Trash with Own, an own cleanup (docs/02 §Protection, I-220)
	// commandVersionV9: a Commit of a rewrite (Rewrite, RewriteStart; docs/04 §6, I-223),
	// a Trash with Expect (I-225) and a Rename with NoReplace (I-224, I-226). Fields are
	// gated by version equality, so a v9 Commit still carries Sums and a v9 Trash writes
	// Own as a byte (in v8 the version was the flag).
	commandVersionV9 = 9
	// commandVersionV10: a SetAttr whose Flags carry the roms library kind (docs/18 §9,
	// I-277); an older binary rejects the third bit.
	commandVersionV10 = 10
	// CodecRoms is the codec every live member must read before a gateway sets the roms
	// kind: a v10 entry forwarded to an older leader is refused as malformed.
	CodecRoms = commandVersionV10
	// CodecRewrite is the codec every live member must read before a gateway sends a
	// codec-9 field: a v9 entry forwarded to an older leader is refused as malformed,
	// not with ErrCodec, so the gateway asks the prober's MinCodec first.
	CodecRewrite = commandVersionV9
	// CodecMirrors is the codec every live member must read before a mirror or a
	// snapshot command is proposed (docs/02 §Codec versions).
	CodecMirrors = commandVersionV7
	// CodecShardSums is the codec every live member must read before a Commit carries
	// Sums (the flusher asks the prober's MinCodec, docs/02 §Codec versions).
	CodecShardSums = commandVersionV5
	// CodecVersion is the newest log entry and snapshot format this binary writes. A
	// node refuses to join a cluster whose leader reports a newer one (docs/12 §6,
	// I-105): it could not decode what the leader writes.
	CodecVersion = 10
)

// SetAttr mask bits.
const (
	AttrMode  uint8 = 1 << 0
	AttrUid   uint8 = 1 << 1
	AttrGid   uint8 = 1 << 2
	AttrAtime uint8 = 1 << 3
	AttrMtime uint8 = 1 << 4
	AttrFlags uint8 = 1 << 5 // Flags follows Now on the wire (codec 4); directories only
)

// Payloads. Every command that the FSM timestamps carries Now (unix nanos) from the
// proposer; the FSM never reads a clock (I-4).
type (
	SetVolume struct {
		ChunkSize uint32
		K, M      uint8
		Now       int64
	}
	SetVolumeMode struct {
		Auto bool
		Now  int64
	}
	MetaSet struct {
		ClusterID [16]byte
		JoinToken string
		CreatedAt int64
	}
	NodeUpsert struct {
		Node Node // Idx 0 allocates; otherwise updates mutable fields of an existing node
	}
	NodeSetState struct {
		Idx   NodeIdx
		State NodeState
		Now   int64
	}
	Create struct {
		Parent   Ino
		Name     string
		Type     FileType // TFile or TSymlink
		Mode     uint32
		Uid, Gid uint32
		Target   string // TSymlink only
		Now      int64
	}
	Mkdir struct {
		Parent   Ino
		Name     string
		Mode     uint32
		Uid, Gid uint32
		Now      int64
	}
	Unlink struct {
		Parent Ino
		Name   string
		Now    int64
	}
	Rmdir struct {
		Parent Ino
		Name   string
		Now    int64
	}
	Rename struct {
		OldParent Ino
		OldName   string
		NewParent Ino
		NewName   string
		Now       int64
		// NoReplace refuses an existing destination with ErrExist (codec 9, I-224, I-226).
		NoReplace bool
	}
	SetAttr struct {
		Ino      Ino
		Mask     uint8
		Mode     uint32
		Uid, Gid uint32
		Atime    int64
		Mtime    int64
		Now      int64
		Flags    uint8 // with AttrFlags: the directory's FlagProtected | FlagNSFW bits
	}
	// Trash moves Parent/Name out of the namespace into the trash (docs/02): a directory
	// with children needs Recursive (a POSIX rmdir is not recursive; a browser delete is).
	Trash struct {
		Parent    Ino
		Name      string
		Recursive bool
		Now       int64
		// Own marks an own cleanup: the SMB handle that created the file deletes it, as a
		// failed copy does, and protection lets that one through (I-220). Codec 8: the
		// version is the flag, no byte is added; codec 9 writes it as a byte.
		Own bool
		// Expect, when not 0, is the inode the proposer means: a name that resolves to
		// another is ErrConflict and nothing changes (codec 9, I-225).
		Expect Ino
	}
	// Restore puts a deleted root back: under NewParent/NewName, or (0, "") its own place.
	Restore struct {
		Ino       Ino
		NewParent Ino
		NewName   string
		Now       int64
	}
	// Purge deletes deleted roots for good: the one root Ino, or every root with
	// DeletedAt < Before, at most Limit inodes per entry (0 is unbounded), post-order.
	Purge struct {
		Ino    Ino
		Before int64
		Limit  uint32
		Now    int64
	}
	Commit struct {
		Ino         Ino
		BaseVersion uint64
		HasTruncate bool   // TruncateTo is meaningful
		TruncateTo  uint64 // lowest size the file had since the last flush; applied first
		NewSize     uint64
		Writes      []ChunkWrite
		NewChunks   []ChunkDesc
		Now         int64
		// Rewrite is the rewrite this Commit continues: the version its RewriteStart
		// produced. RewriteStart begins one (TruncateTo 0, no BaseVersion check). A
		// Commit with neither is a plain write (codec 9, docs/04 §6, I-223).
		Rewrite      uint64
		RewriteStart bool
	}
	SetPlacement struct {
		Hash      Hash
		Placement []NodeIdx
		K, M      uint8
		// Expect, when set, is the placement the proposer planned from: the FSM applies
		// the command only if the chunk still has it, else ErrConflict (I-102). nil
		// skips the check (never written by current code).
		Expect []NodeIdx
	}
	GCDelete struct {
		Before int64
		Limit  uint32 // at most this many victims per entry, in hash order; 0 is unbounded (I-103)
	}
)

// SnapshotBegin records a syncing catalogue row for Mirror at the command's own index
// (docs/16 §3); the mirror's host captures the state there.
type SnapshotBegin struct {
	Mirror NodeIdx
	Name   string
	Auto   bool
	Now    int64
}

// SnapshotEnd ends a sync: with OK the row becomes Complete (Incomplete when Missing > 0)
// and takes the sizes; without it the row is removed. Either way its GC hold ends.
type SnapshotEnd struct {
	ID       SnapID
	OK       bool
	Root     Hash
	Files    uint64
	Bytes    uint64
	NewBytes uint64
	Missing  uint32
	Now      int64
}

// SnapshotDelete is the leader's thinning (docs/16 §4, §5): without Done it marks a row
// Deleting; with Done, sent once the mirror's host has swept it, it removes the row.
type SnapshotDelete struct {
	ID   SnapID
	Done bool
	Now  int64
}

// SnapshotImport records a catalogue row for a manifest the mirror's host found on its
// disk with no row (docs/16 §5): the row takes an ID above both NextSnap and After (the
// largest manifest ID on the disk) and the manifest's own name, time and sizes.
type SnapshotImport struct {
	Mirror   NodeIdx
	After    SnapID
	Name     string
	At       int64
	Auto     bool
	Root     Hash
	Files    uint64
	Bytes    uint64
	NewBytes uint64
	Missing  uint32
	Now      int64
}

// ChunkWrite assigns chunk slot Idx of the committed file; Hash may be HashZero.
type ChunkWrite struct {
	Idx  uint32
	Hash Hash
}

// ChunkDesc registers a new chunk (ignored if the hash already exists: dedup, except
// that Sums fill a chunk that lacked them).
type ChunkDesc struct {
	Hash      Hash
	Size      uint32
	K, M      uint8
	Placement []NodeIdx
	Sums      []Hash // the data shards' SHA-256s, len K, or none (codec 5, I-201)
}

// SetShardSums records the data-shard hashes of a chunk committed without them (the
// leader's backfill, docs/06 §4): the chunk must exist, len(Sums) must be its K, and
// sums already recorded must be equal (ErrConflict otherwise).
type SetShardSums struct {
	Hash Hash
	Sums []Hash
}

// GCVictim is a chunk removed by GCDelete whose shards the leader must now delete.
type GCVictim struct {
	Hash      Hash
	K, M      uint8 // the collected entry's encoding, so shard keys can be derived
	Placement []NodeIdx
}

// Command is one Raft log entry: Op plus exactly one non-nil payload.
type Command struct {
	Op            Op
	SetVolume     *SetVolume
	MetaSet       *MetaSet
	NodeUpsert    *NodeUpsert
	NodeSetState  *NodeSetState
	Create        *Create
	Mkdir         *Mkdir
	Unlink        *Unlink
	Rmdir         *Rmdir
	Rename        *Rename
	SetAttr       *SetAttr
	Commit        *Commit
	SetPlacement  *SetPlacement
	GCDelete      *GCDelete
	SetVolumeMode *SetVolumeMode
	Trash         *Trash
	Restore       *Restore
	Purge         *Purge
	SetShardSums  *SetShardSums
	// docs/16, codec 7
	SnapshotBegin  *SnapshotBegin
	SnapshotEnd    *SnapshotEnd
	SnapshotDelete *SnapshotDelete
	SnapshotImport *SnapshotImport
}

// needsV7 reports whether c is a snapshot command or registers a mirror (docs/16): an
// older binary cannot decode the ops and would place on a mirror row, so the leader
// refuses them while such a member is live (I-184).
// needsV8 reports whether c is an own cleanup, a codec-8 Trash: an older binary would
// apply it as a plain Trash and refuse what this one allows, so the leader refuses the
// entry while such a member is live (I-156, I-220).
func needsV8(c Command) bool {
	return c.Op == OpTrash && c.Trash != nil && c.Trash.Own
}

// needsV9 reports whether c carries a codec-9 field: a Commit of a rewrite, a Trash with
// Expect or a Rename with NoReplace. An older binary would misparse or ignore each, so
// the leader refuses the entry while such a member is live (I-156) and the gateways do
// not send one until every live member reads codec 9 (CodecRewrite).
func needsV9(c Command) bool {
	switch c.Op { //nolint:exhaustive // every other op is version 1–8
	case OpCommit:
		return c.Commit != nil && (c.Commit.Rewrite != 0 || c.Commit.RewriteStart)
	case OpTrash:
		return c.Trash != nil && c.Trash.Expect != 0
	case OpRename:
		return c.Rename != nil && c.Rename.NoReplace
	}
	return false
}

func needsV7(c Command) bool {
	switch c.Op { //nolint:exhaustive // every other op is version 1–6
	case OpSnapshotBegin, OpSnapshotEnd, OpSnapshotDelete, OpSnapshotImport:
		return true
	case OpNodeUpsert:
		return c.NodeUpsert != nil && c.NodeUpsert.Node.Kind == KindMirror
	}
	return false
}

// needsV10 reports whether c sets the roms library kind, a codec-10 field (I-277).
func needsV10(c Command) bool {
	return c.Op == OpSetAttr && c.SetAttr != nil && c.SetAttr.Mask&AttrFlags != 0 && (Inode{Flags: c.SetAttr.Flags}).Library() >= LibraryRoms
}

// needsV6 reports whether c carries the library kind, a codec-6 field: an older binary
// rejects the bits, so the leader refuses the entry while such a member is live (I-210).
func needsV6(c Command) bool {
	return c.Op == OpSetAttr && c.SetAttr != nil && c.SetAttr.Mask&AttrFlags != 0 && c.SetAttr.Flags&FlagLibraryMask != 0
}

// needsV5 reports whether c carries a codec-5 field: SetShardSums, or a Commit whose
// NewChunks carry Sums (docs/04 §4, I-201).
func needsV5(c Command) bool {
	switch c.Op { //nolint:exhaustive // every other op is version 1–4
	case OpSetShardSums:
		return true
	case OpCommit:
		if c.Commit == nil {
			return false
		}
		for i := range c.Commit.NewChunks {
			if len(c.Commit.NewChunks[i].Sums) > 0 {
				return true
			}
		}
	}
	return false
}

// needsV4 reports whether c carries a codec-4 field (docs/02 §Codec versions).
func needsV4(c Command) bool {
	switch c.Op { //nolint:exhaustive // every other op is version 1–3
	case OpTrash, OpRestore, OpPurge:
		return true
	case OpSetAttr:
		return c.SetAttr != nil && c.SetAttr.Mask&AttrFlags != 0
	default:
		return false
	}
}

// Result is what Apply returns for a command.
type Result struct {
	Ino     Ino
	Gen     uint32
	Victims []GCVictim
	SnapID  SnapID // SnapshotBegin's new row
	Version uint64 // a RewriteStart Commit's new version, the gateway's rewrite id (I-223)
	Err     error  // nil or one of the sentinel errors
}

// EncodeCommand serializes c. A command whose payload for c.Op is nil, or whose op is
// unknown, is ErrBadCommand: a programming error on the proposer, reported rather than
// panicking the leader (I-26).
func EncodeCommand(c Command) (b []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			b, err = nil, fmt.Errorf("%w: %v", ErrBadCommand, r)
		}
	}()
	return encodeCommand(c), nil
}

// MustEncodeCommand is EncodeCommand for tests and the harness: it panics on a bad
// command.
func MustEncodeCommand(c Command) []byte {
	b, err := EncodeCommand(c)
	if err != nil {
		panic(err)
	}
	return b
}

// CommandCodec is the codec version the log entry for c carries: the lowest that holds
// its fields (docs/02 §Codec versions). A leader refuses a command whose version a live
// member cannot read (docs/12 §6, I-156).
func CommandCodec(c Command) uint8 {
	v2 := c.Op == OpNodeUpsert && c.NodeUpsert != nil && (c.NodeUpsert.Node.Kind == KindDisk || c.NodeUpsert.Node.Host != 0)
	v3 := (c.Op == OpSetPlacement && c.SetPlacement != nil && c.SetPlacement.Expect != nil) ||
		(c.Op == OpGCDelete && c.GCDelete != nil && c.GCDelete.Limit != 0)
	switch {
	case needsV10(c):
		return commandVersionV10
	case needsV9(c):
		return commandVersionV9
	case needsV8(c):
		return commandVersionV8
	case needsV7(c):
		return commandVersionV7
	case needsV6(c):
		return commandVersionV6
	case needsV5(c):
		return commandVersionV5
	case needsV4(c):
		return commandVersionV4
	case v3:
		return commandVersionV3
	case v2:
		return commandVersionV2
	default:
		return commandVersion
	}
}

func encodeCommand(c Command) []byte {
	w := &writer{b: make([]byte, 0, 64)}
	v := CommandCodec(c)
	v3, v4, v5 := v == commandVersionV3, v == commandVersionV4 || v == commandVersionV6 || v == commandVersionV10, v == commandVersionV5 || v == commandVersionV9
	v9 := v == commandVersionV9
	v2 := v == commandVersionV2 || v == commandVersionV7 // a mirror row carries its Host as a disk row does
	w.u8(v)
	w.u8(uint8(c.Op))
	switch c.Op {
	case OpSetVolume:
		p := mustPayload(c.SetVolume, c.Op)
		w.u32(p.ChunkSize)
		w.u8(p.K)
		w.u8(p.M)
		w.i64(p.Now)
	case OpMetaSet:
		p := mustPayload(c.MetaSet, c.Op)
		w.b = append(w.b, p.ClusterID[:]...)
		w.str(p.JoinToken)
		w.i64(p.CreatedAt)
	case OpNodeUpsert:
		p := mustPayload(c.NodeUpsert, c.Op)
		writeNode(w, &p.Node, v2)
	case OpNodeSetState:
		p := mustPayload(c.NodeSetState, c.Op)
		w.u16(uint16(p.Idx))
		w.u8(uint8(p.State))
		w.i64(p.Now)
	case OpCreate:
		p := mustPayload(c.Create, c.Op)
		w.u64(uint64(p.Parent))
		w.str(p.Name)
		w.u8(uint8(p.Type))
		w.u32(p.Mode)
		w.u32(p.Uid)
		w.u32(p.Gid)
		w.str(p.Target)
		w.i64(p.Now)
	case OpMkdir:
		p := mustPayload(c.Mkdir, c.Op)
		w.u64(uint64(p.Parent))
		w.str(p.Name)
		w.u32(p.Mode)
		w.u32(p.Uid)
		w.u32(p.Gid)
		w.i64(p.Now)
	case OpUnlink:
		p := mustPayload(c.Unlink, c.Op)
		w.u64(uint64(p.Parent))
		w.str(p.Name)
		w.i64(p.Now)
	case OpRmdir:
		p := mustPayload(c.Rmdir, c.Op)
		w.u64(uint64(p.Parent))
		w.str(p.Name)
		w.i64(p.Now)
	case OpRename:
		p := mustPayload(c.Rename, c.Op)
		w.u64(uint64(p.OldParent))
		w.str(p.OldName)
		w.u64(uint64(p.NewParent))
		w.str(p.NewName)
		w.i64(p.Now)
		if v9 {
			w.bool(p.NoReplace)
		}
	case OpSetAttr:
		p := mustPayload(c.SetAttr, c.Op)
		w.u64(uint64(p.Ino))
		w.u8(p.Mask)
		w.u32(p.Mode)
		w.u32(p.Uid)
		w.u32(p.Gid)
		w.i64(p.Atime)
		w.i64(p.Mtime)
		w.i64(p.Now)
		if v4 {
			w.u8(p.Flags)
		}
	case OpCommit:
		p := mustPayload(c.Commit, c.Op)
		w.u64(uint64(p.Ino))
		w.u64(p.BaseVersion)
		w.bool(p.HasTruncate)
		w.u64(p.TruncateTo)
		w.u64(p.NewSize)
		w.uvarint(uint64(len(p.Writes)))
		for _, cw := range p.Writes {
			w.u32(cw.Idx)
			w.hash(cw.Hash)
		}
		w.uvarint(uint64(len(p.NewChunks)))
		for i := range p.NewChunks {
			d := &p.NewChunks[i]
			w.hash(d.Hash)
			w.u32(d.Size)
			w.u8(d.K)
			w.u8(d.M)
			w.nodeIdxs(d.Placement)
			if v5 {
				w.hashes(d.Sums)
			}
		}
		w.i64(p.Now)
		if v9 {
			w.u64(p.Rewrite)
			w.bool(p.RewriteStart)
		}
	case OpSetShardSums:
		p := mustPayload(c.SetShardSums, c.Op)
		w.hash(p.Hash)
		w.hashes(p.Sums)
	case OpSetPlacement:
		p := mustPayload(c.SetPlacement, c.Op)
		w.hash(p.Hash)
		w.nodeIdxs(p.Placement)
		if v3 {
			w.nodeIdxs(p.Expect)
		}
		w.u8(p.K)
		w.u8(p.M)
	case OpGCDelete:
		p := mustPayload(c.GCDelete, c.Op)
		w.i64(p.Before)
		if v3 {
			w.u32(p.Limit)
		}
	case OpSetVolumeMode:
		p := mustPayload(c.SetVolumeMode, c.Op)
		w.bool(p.Auto)
		w.i64(p.Now)
	case OpTrash:
		p := mustPayload(c.Trash, c.Op)
		w.u64(uint64(p.Parent))
		w.str(p.Name)
		w.bool(p.Recursive)
		w.i64(p.Now)
		if v9 {
			w.bool(p.Own)
			w.u64(uint64(p.Expect))
		}
	case OpRestore:
		p := mustPayload(c.Restore, c.Op)
		w.u64(uint64(p.Ino))
		w.u64(uint64(p.NewParent))
		w.str(p.NewName)
		w.i64(p.Now)
	case OpPurge:
		p := mustPayload(c.Purge, c.Op)
		w.u64(uint64(p.Ino))
		w.i64(p.Before)
		w.u32(p.Limit)
		w.i64(p.Now)
	case OpSnapshotBegin:
		p := mustPayload(c.SnapshotBegin, c.Op)
		w.u16(uint16(p.Mirror))
		w.str(p.Name)
		w.bool(p.Auto)
		w.i64(p.Now)
	case OpSnapshotEnd:
		p := mustPayload(c.SnapshotEnd, c.Op)
		w.u32(uint32(p.ID))
		w.bool(p.OK)
		w.hash(p.Root)
		w.u64(p.Files)
		w.u64(p.Bytes)
		w.u64(p.NewBytes)
		w.u32(p.Missing)
		w.i64(p.Now)
	case OpSnapshotDelete:
		p := mustPayload(c.SnapshotDelete, c.Op)
		w.u32(uint32(p.ID))
		w.bool(p.Done)
		w.i64(p.Now)
	case OpSnapshotImport:
		p := mustPayload(c.SnapshotImport, c.Op)
		w.u16(uint16(p.Mirror))
		w.u32(uint32(p.After))
		w.str(p.Name)
		w.i64(p.At)
		w.bool(p.Auto)
		w.hash(p.Root)
		w.u64(p.Files)
		w.u64(p.Bytes)
		w.u64(p.NewBytes)
		w.u32(p.Missing)
		w.i64(p.Now)
	default:
		panic(fmt.Sprintf("meta: EncodeCommand: unknown op %d", c.Op))
	}
	return w.b
}

func mustPayload[T any](p *T, op Op) *T {
	if p == nil {
		panic(fmt.Sprintf("meta: EncodeCommand: op %d without payload", op))
	}
	return p
}

// DecodeCommand parses one log entry. Any malformation (unknown version or op, short
// input, trailing bytes, out-of-range lengths) is an error; the FSM maps it to
// ErrBadCommand identically on every node.
func DecodeCommand(b []byte) (Command, error) {
	r := &reader{b: b}
	var c Command
	v := r.u8()
	if r.err == nil && (v < commandVersion || v > commandVersionV10) {
		return c, fmt.Errorf("meta: unsupported command version %d", v)
	}
	c.Op = Op(r.u8())
	if r.err != nil {
		return c, r.err
	}
	// a version is legal only for the ops that need it, so an entry never smuggles a
	// field an older reader would misparse (docs/01 §Binary codec)
	var legal bool
	switch v {
	case commandVersionV2:
		legal = c.Op == OpNodeUpsert
	case commandVersionV3:
		legal = c.Op == OpSetPlacement || c.Op == OpGCDelete
	case commandVersionV4:
		legal = c.Op == OpTrash || c.Op == OpRestore || c.Op == OpPurge || c.Op == OpSetAttr
	case commandVersionV5:
		legal = c.Op == OpSetShardSums || c.Op == OpCommit
	case commandVersionV6:
		legal = c.Op == OpSetAttr
	case commandVersionV7:
		legal = c.Op == OpSnapshotBegin || c.Op == OpSnapshotEnd || c.Op == OpSnapshotDelete || c.Op == OpSnapshotImport || c.Op == OpNodeUpsert
	case commandVersionV8:
		legal = c.Op == OpTrash
	case commandVersionV9:
		legal = c.Op == OpCommit || c.Op == OpTrash || c.Op == OpRename
	case commandVersionV10:
		legal = c.Op == OpSetAttr
	default:
		legal = c.Op != OpTrash && c.Op != OpRestore && c.Op != OpPurge && c.Op != OpSetShardSums &&
			c.Op != OpSnapshotBegin && c.Op != OpSnapshotEnd && c.Op != OpSnapshotDelete && c.Op != OpSnapshotImport
	}
	if !legal {
		return c, fmt.Errorf("meta: op %d in command version %d", c.Op, v)
	}
	switch c.Op {
	case OpSetVolume:
		c.SetVolume = &SetVolume{ChunkSize: r.u32(), K: r.u8(), M: r.u8(), Now: r.i64()}
	case OpMetaSet:
		p := &MetaSet{}
		copy(p.ClusterID[:], r.take(16))
		p.JoinToken = r.str()
		p.CreatedAt = r.i64()
		c.MetaSet = p
	case OpNodeUpsert:
		c.NodeUpsert = &NodeUpsert{Node: readNode(r, v >= commandVersionV2)}
		if r.err == nil && (c.NodeUpsert.Node.Kind == KindMirror) != (v == commandVersionV7) {
			return c, fmt.Errorf("meta: a mirror row is written in command version 7 and only it (got version %d)", v) // never smuggled to an older reader (I-184)
		}
	case OpNodeSetState:
		c.NodeSetState = &NodeSetState{Idx: NodeIdx(r.u16()), State: NodeState(r.u8()), Now: r.i64()}
	case OpCreate:
		c.Create = &Create{Parent: Ino(r.u64()), Name: r.str(), Type: FileType(r.u8()), Mode: r.u32(), Uid: r.u32(), Gid: r.u32(), Target: r.str(), Now: r.i64()}
	case OpMkdir:
		c.Mkdir = &Mkdir{Parent: Ino(r.u64()), Name: r.str(), Mode: r.u32(), Uid: r.u32(), Gid: r.u32(), Now: r.i64()}
	case OpUnlink:
		c.Unlink = &Unlink{Parent: Ino(r.u64()), Name: r.str(), Now: r.i64()}
	case OpRmdir:
		c.Rmdir = &Rmdir{Parent: Ino(r.u64()), Name: r.str(), Now: r.i64()}
	case OpRename:
		c.Rename = &Rename{OldParent: Ino(r.u64()), OldName: r.str(), NewParent: Ino(r.u64()), NewName: r.str(), Now: r.i64()}
		if v == commandVersionV9 {
			c.Rename.NoReplace = r.bool()
			if r.err == nil && !c.Rename.NoReplace {
				return c, fmt.Errorf("meta: Rename in command version 9 without NoReplace") // canonical bytes (I-22)
			}
		}
	case OpSetAttr:
		c.SetAttr = &SetAttr{Ino: Ino(r.u64()), Mask: r.u8(), Mode: r.u32(), Uid: r.u32(), Gid: r.u32(), Atime: r.i64(), Mtime: r.i64(), Now: r.i64()}
		if v == commandVersionV4 || v == commandVersionV6 || v == commandVersionV10 {
			c.SetAttr.Flags = r.u8()
			if r.err == nil && c.SetAttr.Mask&AttrFlags == 0 {
				return c, fmt.Errorf("meta: SetAttr in command version %d without AttrFlags", v)
			}
			if r.err == nil && v < commandVersionV6 && c.SetAttr.Flags&FlagLibraryMask != 0 {
				return c, fmt.Errorf("meta: library bits in command version %d", v) // never written; an older reader would reject them (I-210)
			}
			if k := (Inode{Flags: c.SetAttr.Flags}).Library(); r.err == nil && (k >= LibraryRoms) != (v == commandVersionV10) {
				return c, fmt.Errorf("meta: library kind %d in command version %d", k, v) // roms is version 10 and only it (I-277)
			}
		}
	case OpCommit:
		p := &Commit{Ino: Ino(r.u64()), BaseVersion: r.u64(), HasTruncate: r.bool(), TruncateTo: r.u64(), NewSize: r.u64()}
		if n := r.length(maxListLen); n > 0 {
			p.Writes = make([]ChunkWrite, n)
			for i := range p.Writes {
				p.Writes[i] = ChunkWrite{Idx: r.u32(), Hash: r.hash()}
			}
		}
		if n := r.length(maxListLen); n > 0 {
			p.NewChunks = make([]ChunkDesc, n)
			for i := range p.NewChunks {
				p.NewChunks[i] = ChunkDesc{Hash: r.hash(), Size: r.u32(), K: r.u8(), M: r.u8(), Placement: r.nodeIdxs()}
				if v == commandVersionV5 || v == commandVersionV9 {
					p.NewChunks[i].Sums = r.hashes()
				}
			}
		}
		p.Now = r.i64()
		if v == commandVersionV9 {
			p.Rewrite, p.RewriteStart = r.u64(), r.bool()
			if r.err == nil && p.Rewrite == 0 && !p.RewriteStart {
				return c, fmt.Errorf("meta: Commit in command version 9 without a rewrite") // canonical bytes (I-22)
			}
		}
		c.Commit = p
	case OpSetShardSums:
		c.SetShardSums = &SetShardSums{Hash: r.hash(), Sums: r.hashes()}
	case OpSetPlacement:
		p := &SetPlacement{Hash: r.hash(), Placement: r.nodeIdxs()}
		if v == commandVersionV3 {
			p.Expect = r.nodeIdxs()
			if p.Expect == nil {
				p.Expect = []NodeIdx{} // written as set; stays set (an empty expectation never matches a stripe)
			}
		}
		p.K, p.M = r.u8(), r.u8()
		c.SetPlacement = p
	case OpGCDelete:
		c.GCDelete = &GCDelete{Before: r.i64()}
		if v == commandVersionV3 {
			c.GCDelete.Limit = r.u32()
		}
	case OpSetVolumeMode:
		c.SetVolumeMode = &SetVolumeMode{Auto: r.bool(), Now: r.i64()}
	case OpTrash:
		c.Trash = &Trash{Parent: Ino(r.u64()), Name: r.str(), Recursive: r.bool(), Now: r.i64(), Own: v == commandVersionV8}
		if v == commandVersionV9 {
			c.Trash.Own, c.Trash.Expect = r.bool(), Ino(r.u64())
			if r.err == nil && c.Trash.Expect == 0 {
				return c, fmt.Errorf("meta: Trash in command version 9 without Expect") // canonical bytes (I-22)
			}
		}
	case OpRestore:
		c.Restore = &Restore{Ino: Ino(r.u64()), NewParent: Ino(r.u64()), NewName: r.str(), Now: r.i64()}
	case OpPurge:
		c.Purge = &Purge{Ino: Ino(r.u64()), Before: r.i64(), Limit: r.u32(), Now: r.i64()}
	case OpSnapshotBegin:
		c.SnapshotBegin = &SnapshotBegin{Mirror: NodeIdx(r.u16()), Name: r.str(), Auto: r.bool(), Now: r.i64()}
	case OpSnapshotEnd:
		c.SnapshotEnd = &SnapshotEnd{ID: SnapID(r.u32()), OK: r.bool(), Root: r.hash(), Files: r.u64(), Bytes: r.u64(), NewBytes: r.u64(), Missing: r.u32(), Now: r.i64()}
	case OpSnapshotDelete:
		c.SnapshotDelete = &SnapshotDelete{ID: SnapID(r.u32()), Done: r.bool(), Now: r.i64()}
	case OpSnapshotImport:
		c.SnapshotImport = &SnapshotImport{
			Mirror: NodeIdx(r.u16()), After: SnapID(r.u32()), Name: r.str(), At: r.i64(), Auto: r.bool(), Root: r.hash(),
			Files: r.u64(), Bytes: r.u64(), NewBytes: r.u64(), Missing: r.u32(), Now: r.i64(),
		}
	default:
		return c, fmt.Errorf("meta: unknown op %d", c.Op)
	}
	if err := r.done(); err != nil {
		return Command{}, err
	}
	return c, nil
}

// writeNode encodes a node; withHost appends Host (command version 2, snapshot version 3).
func writeNode(w *writer, n *Node, withHost bool) {
	w.u16(uint16(n.Idx))
	w.str(n.ID)
	w.str(n.Name)
	w.u8(uint8(n.Kind))
	w.u8(uint8(n.State))
	w.str(n.TSStableID)
	w.str(n.Addr)
	w.u16(n.RaftPort)
	w.u16(n.RPCPort)
	w.bool(n.Voter)
	w.u64(n.Capacity)
	w.str(n.Version)
	w.i64(n.JoinedAt)
	w.str(n.S3Endpoint)
	w.str(n.S3Bucket)
	w.str(n.S3Prefix)
	w.str(n.S3Region)
	w.bool(n.S3PathStyle)
	if withHost {
		w.u16(uint16(n.Host))
	}
}

func readNode(r *reader, withHost bool) Node {
	n := Node{
		Idx: NodeIdx(r.u16()), ID: r.str(), Name: r.str(), Kind: NodeKind(r.u8()), State: NodeState(r.u8()),
		TSStableID: r.str(), Addr: r.str(), RaftPort: r.u16(), RPCPort: r.u16(), Voter: r.bool(), Capacity: r.u64(),
		Version: r.str(), JoinedAt: r.i64(),
		S3Endpoint: r.str(), S3Bucket: r.str(), S3Prefix: r.str(), S3Region: r.str(), S3PathStyle: r.bool(),
	}
	if withHost {
		n.Host = NodeIdx(r.u16())
	}
	return n
}

// EncodeResult serializes a Result for leader forwarding. Err must be nil or a sentinel.
func EncodeResult(res Result) []byte {
	w := &writer{}
	w.u8(ErrorCode(res.Err))
	w.u64(uint64(res.Ino))
	w.u32(res.Gen)
	w.uvarint(uint64(len(res.Victims)))
	for i := range res.Victims {
		w.hash(res.Victims[i].Hash)
		w.u8(res.Victims[i].K)
		w.u8(res.Victims[i].M)
		w.nodeIdxs(res.Victims[i].Placement)
	}
	if res.SnapID != 0 || res.Version != 0 {
		// appended only when set: only SnapshotBegin sets SnapID (codec 7) and only a
		// RewriteStart Commit sets Version (codec 9), so a node that reads the extra
		// fields is the only kind that ever receives them
		w.u32(uint32(res.SnapID))
	}
	if res.Version != 0 {
		w.u64(res.Version)
	}
	return w.b
}

// DecodeResult is the inverse of EncodeResult.
func DecodeResult(b []byte) (Result, error) {
	r := &reader{b: b}
	var res Result
	code := r.u8()
	if r.err != nil {
		return res, r.err
	}
	err, ok := ErrorFromCode(code)
	if !ok {
		return res, fmt.Errorf("meta: unknown error code %d", code)
	}
	res.Err = err
	res.Ino = Ino(r.u64())
	res.Gen = r.u32()
	if n := r.length(maxListLen); n > 0 {
		res.Victims = make([]GCVictim, n)
		for i := range res.Victims {
			res.Victims[i] = GCVictim{Hash: r.hash(), K: r.u8(), M: r.u8(), Placement: r.nodeIdxs()}
		}
	}
	if r.err == nil && r.pos < len(r.b) {
		res.SnapID = SnapID(r.u32())
	}
	if r.err == nil && r.pos < len(r.b) {
		res.Version = r.u64()
	}
	if err := r.done(); err != nil {
		return Result{}, err
	}
	return res, nil
}
