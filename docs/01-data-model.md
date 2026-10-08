# 01 — Data model

Everything the cluster agrees on lives in one in-memory `meta.State`, replicated by Raft
(see [02](02-consistency-and-raft.md)). Everything else is derived, local, or content
addressed. This document fixes the types, the chunking rules, and every byte format that
crosses a process or disk boundary.

## Core types (`internal/meta/types.go`)

```go
type Hash [32]byte            // SHA-256 of the logical chunk bytes
var HashZero Hash             // sentinel: an all-zero chunk; never stored anywhere

type NodeIdx uint16           // dense index into State.Nodes; 0 = "no node" (missing slot)
type Ino     uint64           // root = 1; monotonic, never reused
type FileType uint8           // TFile = 1, TDir = 2, TSymlink = 3

type Inode struct {
    Ino      Ino
    Gen      uint32           // low 32 bits of the Raft index of the creating command
    Type     FileType
    Mode     uint32           // permission bits only (0o7777)
    Uid, Gid uint32
    Size     uint64
    Atime, Mtime, Ctime int64 // unix nanoseconds, supplied by the proposer (never the FSM)
    Nlink    uint16           // 1 for files/symlinks; 2 + number of subdirectories for dirs
    Version  uint64           // files: bumps on every Commit (data change), Commit's optimistic check;
                              // directories: bumps on every change of the name table (READDIR cookie verifier)
    Parent   Ino              // 0 for root; single parent (no hard links)
    Name     string           // name within Parent; "" for root
    Chunks   []Hash           // TFile only; len == ceil(Size/ChunkSize); copy-on-write slice
    Target   string           // TSymlink only
    Children map[string]Ino   // TDir only
}

type Chunk struct {
    Hash         Hash
    Size         uint32       // logical bytes, 1..ChunkSize
    K, M         uint8        // the policy this chunk was encoded with
    Refs         uint32       // number of (inode, index) positions referencing it
    Placement    []NodeIdx    // len K+M; Placement[i] holds shard i; 0 = missing
    TombstonedAt int64        // unix nanos when Refs reached 0; 0 while live
}

type NodeKind  uint8          // KindDevice = 1, KindS3 = 2
type NodeState uint8          // Active = 1, Draining = 2, Removed = 3

type Node struct {           // the ONE node struct; 02 and 10 refer to it, never redefine it
    Idx        NodeIdx
    ID         string         // cluster node ID: random 16 hex chars; = raft.ServerID for devices
    Name       string
    Kind       NodeKind
    State      NodeState
    TSStableID string         // device: Tailscale stable node ID, used for RPC authorization
    Addr       string         // device: current Tailscale IPv4 (string form)
    RaftPort, RPCPort uint16  // device only
    Voter      bool           // device only
    Capacity   uint64         // shard-byte budget; also the placement weight; derived from the
                              // data directory's free space unless pinned by node.capacity (I-74)
    Version    string         // software version last reported (device only)
    JoinedAt   int64
    // s3 only (credentials are NOT here; see 10-s3.md)
    S3Endpoint, S3Bucket, S3Prefix, S3Region string
    S3PathStyle bool
}

type Volume struct {
    ChunkSize     uint32      // default 1 MiB; fixed for the life of the volume
    K, M          uint8       // current policy for NEW chunks
    PolicyVersion uint32      // bumped by every SetVolume that changes K or M
    Auto          bool        // the leader picks K, M from the member table (03); set by init,
                              // cleared by `volume set-redundancy --k --m`, restored by `--auto`
}

type Meta struct {
    ClusterID [16]byte        // random at init; checked on every Raft stream and RPC
    JoinToken string          // 8 chars; rotated with `tailraid token rotate`
    CreatedAt int64
}

type State struct {
    Vol     Volume
    Meta    Meta
    NextIno Ino
    NextNode NodeIdx
    Inodes  map[Ino]*Inode
    Chunks  map[Hash]*Chunk
    Nodes   map[NodeIdx]*Node
    Tombs   map[Hash]struct{} // index of Chunks with Refs == 0
    Applied uint64            // last applied Raft index
}
```

Rules:

- `Ino` values are allocated from `NextIno` and never reused. `Gen` is recorded at creation.
  Together `(Ino, Gen)` identify a file for the life of the volume (I-3). NFS handles encode
  exactly this pair.
- A directory's `Children` map is the only directory listing. Directories are first-class
  inodes; an empty directory persists (the predecessor derived directories from key
  prefixes and lost them).
- `Inode.Chunks` is treated as immutable: every Commit builds a new slice. Readers that hold
  an `Inode` copy therefore see a consistent chunk list without locking.
- `Node.Idx` is allocated from `NextNode` and never reused, so `Placement` entries remain
  meaningful after a node is removed (they point at a `Removed` node, which the repairer
  treats like a missing slot).

## Chunking

- **Fixed-size chunks.** `ChunkSize` defaults to 1 MiB and is set at `tailraid init`. Byte
  `off` of a file lives in chunk `idx = off / ChunkSize` at chunk offset `off % ChunkSize`.
  Random-offset NFS writes modify one or two chunks and never re-chunk the file. Content
  defined chunking is out of scope.
- **Chunk count.** `len(Inode.Chunks) == ceil(Size / ChunkSize)`; the last chunk has logical
  length `Size - (len-1)*ChunkSize`. An empty file has `Chunks == nil` (I-1).
- **Holes.** `HashZero` in `Chunks[i]` means chunk `i` reads as zeros. It is produced by
  writes past EOF, `Truncate` upward, and by flushing a chunk whose assembled bytes are all
  zero. `HashZero` is never stored in `State.Chunks`, never uploaded, never ref-counted.
- **Short chunk under a longer file.** After `Truncate` upward over a short final chunk, the
  chunk entry keeps its original `Size`; readers zero-fill from `Chunk.Size` to the logical
  chunk length. Truncate downward to a non-chunk-aligned size requires the new tail chunk to
  be rewritten in the same Commit (`ErrNeedTailRewrite`, see 02).
- **Hashing.** `Hash = SHA-256(logical chunk bytes)` over exactly `Chunk.Size` bytes, with no
  padding included (I-2). The same bytes at different offsets or in different files produce
  the same hash and share one `Chunk` entry; that is the dedup mechanism.
- **Small chunks.** A chunk with `Size <= 64 KiB` is encoded with `K = 1` (i.e. `M+1` full
  replicas). Larger chunks use the volume's `(K, M)`. The chosen `(K, M)` is recorded in the
  `Chunk` entry, so the volume policy may change at any time without touching existing
  chunks ([03](03-erasure-coding-and-placement.md)).

## Shards

- **Key.** `ShardKey = hex(Hash) + "." + K + "-" + M + "." + idx`, e.g.
  `3f2a…9c.4-2.0` … `3f2a…9c.4-2.5` for a 4+2 chunk. At most 76 characters. Used as the
  RPC path element, the S3 object key suffix, and the local file name. The encoding is part
  of the key so that shards of the same chunk under two policies (during a
  `volume set-redundancy` migration) never collide; `3f2a…9c.2-1.0` and `3f2a…9c.3-2.0`
  are different files.
- **Length.** All `K+M` shards of a chunk have the same length
  `ShardLen(Size, K) = roundUp(ceil(Size/K), 64)`; the data shards are the chunk zero-padded
  to `K*ShardLen` and split. Padding is never stored in metadata; decoding trims to `Size`.
- **Integrity.** A shard's own hash is not in Raft (that would cost ~250 B per chunk instead
  of ~50 B). Instead each shard file carries a header with `SHA-256(payload)`, verified on
  every read, and `PUT /v1/shards/{key}` carries `X-Shard-SHA256`, verified by the receiver
  before the file becomes visible. The decoded chunk is then verified against `Hash`.
- **Duplicate PUT.** If the key already exists, the receiver compares `X-Shard-SHA256` with
  the existing header hash: equal ⇒ `200`, mtime refreshed, no rewrite; different ⇒ the new
  payload replaces the file atomically (temp + rename) and `200` is returned. A duplicate PUT
  therefore always leaves the file equal to the payload the sender holds.

### Shard file format (`internal/chunk`)

```
offset  size  field
0       4     magic "TRS1"
4       4     payloadLen uint32 LE
8       32    sha256(payload)
40      n     payload (the shard bytes)
```

Path: `<data_dir>/shards/<hh>/<hex>.<k>-<m>.<idx>` where `hh` = first two hex characters
of the chunk hash. Written as `<dir>/.tmp-<random>` → `write` → `fsync` → `close` →
`rename` → `fsync(dir)`; `Put` returns only after the directory fsync, so a PUT ack means the
shard survives power loss. `Store.Open` deletes every `.tmp-*` file it finds, regardless of
age (a temp file can only be an interrupted write). A header or hash mismatch on read returns
`ErrCorrupt`; the file is moved to `quarantine/` and the reader falls back to other shards.
The same layout is the object layout used for S3 storage nodes
(`<prefix>/shards/<hh>/<hex>.<k>-<m>.<idx>`), so a bucket and a local shard directory are
the same "volume format".

### Shard store API (`internal/chunk`)

```go
func Open(fsys sysfs.FS, root string, capacity int64) (*Store, error) // sweeps temps, seeds Used
func (s *Store) Put(key ShardKey, payload []byte) (added int64, err error) // ErrFull before writing
func (s *Store) Get(key ShardKey) ([]byte, error)                        // verifies header; ErrCorrupt quarantines
func (s *Store) Open(key ShardKey) (io.ReadCloser, int64, error)         // verified reader for the RPC server
func (s *Store) Has(key ShardKey) bool
func (s *Store) Delete(key ShardKey) (int64, error)                      // idempotent
func (s *Store) DeleteIfOlder(key ShardKey, than time.Time) (int64, error) // GC/sweep fence (06)
func (s *Store) Used() int64
func (s *Store) Capacity() int64
func (s *Store) Walk(fn func(key ShardKey, size int64, mtime time.Time) error) error
```

### Local chunk cache

`<data_dir>/cache/<hh>/<hex>` holds decoded chunks (not shards) as an LRU bounded by bytes.
Entries are verified against their hash on read; a mismatch evicts the entry. The cache is
purely local and may be deleted at any time.

### Gateway WAL

`<data_dir>/staging/<ino>.wal` holds dirty extents for an inode between NFS WRITE and
cluster flush. Record format and replay rules are in [04](04-write-path-and-durability.md).

## Binary codec (`internal/meta/codec.go`)

One hand-written little-endian codec serves Raft log entries and snapshots. No reflection,
no third-party serialization.

Primitives: `u8`, `u16 LE`, `u32 LE`, `u64 LE`, `uvarint` (Go `encoding/binary`), `bytes`
(uvarint length + raw), `string` (as bytes), `hash` (32 raw bytes).

**Command frame** (one Raft log entry):

```
u8      codec version (1)
u8      Op
...     payload fields in the fixed order documented for that Op in 02
```

Unknown codec version or trailing bytes ⇒ `DecodeCommand` error; the FSM returns
`ErrBadCommand` in `Result.Err` and applies nothing (deterministically on every node).

**Snapshot frame** (`EncodeState`):

```
4 bytes  magic "TRM1"
u8       codec version (2; a version-1 snapshot is still read, with Auto = false)
u64      Applied
section  Volume   {ChunkSize u32, K u8, M u8, PolicyVersion u32, Auto bool}
section  Meta     {ClusterID 16B, JoinToken string, CreatedAt i64}
u64      NextIno, u16 NextNode
section  Nodes    uvarint count, then each Node (fields in struct order; strings as bytes)
section  Chunks   uvarint count, then each Chunk {hash, Size u32, K u8, M u8, Refs u32,
                  uvarint len(Placement), Placement u16..., TombstonedAt i64}
section  Inodes   uvarint count, then each Inode {Ino, Gen, Type, Mode, Uid, Gid, Size,
                  Atime, Mtime, Ctime, Nlink, Version, Parent, Name,
                  TFile: uvarint len(Chunks) + hashes; TSymlink: Target;
                  TDir: uvarint len(Children) + (name, Ino) pairs sorted by name}
```

Maps are emitted in sorted key order (inos ascending, hashes bytewise, node idx ascending,
child names bytewise) so two FSMs with equal state produce byte-identical snapshots. `Tombs`
is not serialized; it is rebuilt from `Chunks` with `Refs == 0` on decode.

## Memory model

| Item | Approx. bytes |
|---|---|
| `Inode` struct + map slot + short name | ~200 |
| per 1 MiB of file data (one `Hash` in `Chunks`) | 32 |
| per directory child (`Children` entry) | ~60 |
| `Chunk` struct + map slot + `Placement` (K+M ≤ 8) | ~110 |

Rule of thumb: 1 M files + 1 TB unique data ≈ 200 MB + 110 MB ≈ **320 MB** of resident
metadata per node. Soft documented limits: 2 M inodes, 4 TB unique data per volume.
Snapshot clone (for `raft.FSM.Snapshot`) of 1 M inodes takes roughly 100–300 ms.

## NFS file handle

```
offset  size  field
0       4     magic 0x54524844 ("TRHD"), big-endian
4       4     Gen uint32 big-endian
8       8     Ino uint64 big-endian
```

16 bytes, fixed, big-endian throughout (network order; it is an opaque wire value). `FromHandle` looks up `Ino`; a missing inode or a `Gen` mismatch returns
`NFS3ERR_STALE`. Handles are therefore stable across renames, gateway restarts, and across
gateways on different devices (they encode cluster-wide identity, not a path).

## Node budget

A device's `Capacity` is its shard-byte budget: the local shard store refuses a PUT that
would exceed it (`ErrFull`, HTTP 507) and placement weights by it. Unless `node.capacity`
is set in the config (a pin), the budget is **derived**: `used + free − reserve` of the
filesystem holding the data directory, with `reserve = max(10 % of the filesystem, 8 GiB)`
so the volume never drives the disk to `ENOSPC`. `init` and `join` derive it before the
member record is written; `serve` re-derives it on every maintenance tick (one minute),
sets the store's cap at once, and proposes `set_capacity` for itself through the leader
only when the value moved by more than `max(5 %, 1 GiB)`, so placement weights stay still
while a disk breathes. `Capacity == 0` (a full disk) takes the node out of the candidate
set without touching what it already holds.

## Invariants

- **I-1** — For every `TFile` inode, `len(Chunks) == ceil(Size/ChunkSize)` and the file's
  content equals `concat(chunk_0 … chunk_{n-1})[:Size]`, where a `HashZero` chunk reads as
  zeros and a chunk shorter than its logical slot is zero-filled to the slot length.
- **I-2** — Every entry in `State.Chunks` is keyed by `SHA-256` of exactly its `Size` logical
  bytes (no padding), and `HashZero` is never present in `State.Chunks`, never uploaded, and
  never ref-counted.
- **I-3** — An `(Ino, Gen)` pair, once assigned, is never assigned to a different inode;
  `Ino` values are never reused after `Unlink`/`Rmdir`.
- **I-22** — Encoding is canonical: two `State` values that are equal produce byte-identical
  `EncodeState` output, and `DecodeState(EncodeState(s)) == s`.
- **I-66** — A directory entry name is 1 to `MaxNameLen` (255) bytes and contains neither
  `/` nor NUL; `Create`, `Mkdir`, `Symlink` and `Rename` reject anything else before touching
  state (`ErrNameTooLong` → `ENAMETOOLONG` / `NFS3ERR_NAMETOOLONG`, otherwise `ErrBadCommand`).

Shard-file integrity (header hash matches payload, temp files never visible) is **I-11**,
defined in `04-write-path-and-durability.md`.

- **I-74** — A device's derived budget is `used + free − reserve` of the data directory's
  filesystem (reserve `max(10 %, 8 GiB)`), re-derived every maintenance tick with the
  store's cap following at once and a `set_capacity` proposal only past the hysteresis
  `max(5 %, 1 GiB)`; a node pinned by `node.capacity` is never re-derived.

## Tests

| Invariant | Tests |
|---|---|
| I-1 | `TestChunkCount_NoOverflow`, `TestFSM_Commit_NewFileAndOverwrite`, `TestMount_Workload`, `TestVFS_OracleRandomOps`, `TestVFS_TruncateThenGrow_OldBytesDoNotResurface`, `TestVFS_ZeroByteFile_NoWALNoCommit` |
| I-2 | `TestFSM_Commit_Errors`, `TestFlusher_DedupLiveAndTombstoned_NoUpload`, `TestReader_DecodedHashMismatch_EIO` |
| I-3 | `TestFSM_InoGenNotReusedAfterUnlink`, `TestInoGen_RootIsOne_ZeroIsNever` |
| I-22 | `FuzzDecodeCommand`, `FuzzDecodeState`, `TestCodec_CanonicalOrdering`, `TestCodec_ReadsVersion1Snapshot`, `TestCodec_StateRoundTrip`, `TestEncodeCommand_Golden`, `TestEncodeState_Canonical` |
| I-74 | `TestStore_Budget_Boundaries`, `TestStore_BudgetChanged_Hysteresis`, `TestStore_SetCapacity_CapFollows`, `TestOS_Free`, `TestBudget_PinnedOrDerived`, `TestConfig_CapacityAuto` |
| I-66 | `TestFSM_Create_NameBoundaries`, `TestVFS_NameTooLong_ENAMETOOLONG` |
