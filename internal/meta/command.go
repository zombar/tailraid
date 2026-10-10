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
)

// commandVersion is the codec version of a log entry. Version 2 is written only for a
// NodeUpsert that carries a disk (its Node has Host), so a cluster without disks writes
// exactly what an older node reads (docs/12 §6); every version is read.
const (
	commandVersion   = 1
	commandVersionV2 = 2
	commandVersionV3 = 3 // SetPlacement.Expect and GCDelete.Limit
	commandVersionV4 = 4 // Trash, Restore, Purge and a SetAttr carrying Flags (docs/02)
	// CodecVersion is the newest log entry and snapshot format this binary writes. A
	// node refuses to join a cluster whose leader reports a newer one (docs/12 §6,
	// I-105): it could not decode what the leader writes.
	CodecVersion = 4
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

// ChunkWrite assigns chunk slot Idx of the committed file; Hash may be HashZero.
type ChunkWrite struct {
	Idx  uint32
	Hash Hash
}

// ChunkDesc registers a new chunk (ignored if the hash already exists: dedup).
type ChunkDesc struct {
	Hash      Hash
	Size      uint32
	K, M      uint8
	Placement []NodeIdx
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
	Err     error // nil or one of the sentinel errors
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

func encodeCommand(c Command) []byte {
	w := &writer{b: make([]byte, 0, 64)}
	v2 := c.Op == OpNodeUpsert && c.NodeUpsert != nil && (c.NodeUpsert.Node.Kind == KindDisk || c.NodeUpsert.Node.Host != 0)
	v3 := (c.Op == OpSetPlacement && c.SetPlacement != nil && c.SetPlacement.Expect != nil) ||
		(c.Op == OpGCDelete && c.GCDelete != nil && c.GCDelete.Limit != 0)
	v4 := needsV4(c)
	switch {
	case v4:
		w.u8(commandVersionV4)
	case v3:
		w.u8(commandVersionV3)
	case v2:
		w.u8(commandVersionV2)
	default:
		w.u8(commandVersion)
	}
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
		}
		w.i64(p.Now)
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
	if r.err == nil && v != commandVersion && v != commandVersionV2 && v != commandVersionV3 && v != commandVersionV4 {
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
	default:
		legal = c.Op != OpTrash && c.Op != OpRestore && c.Op != OpPurge
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
	case OpSetAttr:
		c.SetAttr = &SetAttr{Ino: Ino(r.u64()), Mask: r.u8(), Mode: r.u32(), Uid: r.u32(), Gid: r.u32(), Atime: r.i64(), Mtime: r.i64(), Now: r.i64()}
		if v == commandVersionV4 {
			c.SetAttr.Flags = r.u8()
			if r.err == nil && c.SetAttr.Mask&AttrFlags == 0 {
				return c, fmt.Errorf("meta: SetAttr in command version %d without AttrFlags", v)
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
			}
		}
		p.Now = r.i64()
		c.Commit = p
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
		c.Trash = &Trash{Parent: Ino(r.u64()), Name: r.str(), Recursive: r.bool(), Now: r.i64()}
	case OpRestore:
		c.Restore = &Restore{Ino: Ino(r.u64()), NewParent: Ino(r.u64()), NewName: r.str(), Now: r.i64()}
	case OpPurge:
		c.Purge = &Purge{Ino: Ino(r.u64()), Before: r.i64(), Limit: r.u32(), Now: r.i64()}
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
	if err := r.done(); err != nil {
		return Result{}, err
	}
	return res, nil
}
