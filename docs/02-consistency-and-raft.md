# 02 — Consistency and Raft

All metadata — the inode tree, the chunk table with placement and refcounts, the node table,
the volume policy and the cluster meta — is one deterministic state machine replicated with
`hashicorp/raft`. Chunk *data* is immutable and content-addressed, so it needs no consensus:
a shard either exists under its hash or it does not. This split is what keeps the design
small: Raft carries ~100 bytes per metadata operation; bulk bytes go point to point.

## What lives where

| In Raft (replicated, durable) | Local to a node (derived or private) |
|---|---|
| `State` from 01: inodes, chunk table, node table, volume policy, cluster meta | shard files, chunk cache, gateway WAL |
| placement of every chunk | liveness (`/health` probes), RTT ranking |
| node states Active/Draining/Removed, capacity, voter flag | S3 credentials, access keys, config |
| join token, cluster ID | Raft log and snapshots themselves (`raft.db`, `snapshots/`) |

## Node identity

- **Cluster node ID**: 16 random hex characters generated once and persisted at
  `<data_dir>/node_id`. It is the `raft.ServerID`. It never changes while the data directory
  exists; this is what Raft requires.
- **Tailscale stable ID**: recorded in `Node.TSStableID` and used only to authorize RPCs
  ([08](08-security.md)). It may change on re-auth; that is handled as a member update, not an
  identity change.
- **Address**: `Node.Addr` is the device's Tailscale IPv4. Raft addresses are `Addr:RaftPort`,
  not MagicDNS names (DNS can be down when tailscaled is down). On every probe cycle a node
  compares `tailscale status` Self IP with its own `Node.Addr`; if different it asks the
  leader to update both the Raft configuration (`AddVoter`/`AddNonvoter` with the same ID and
  the new address updates in place) and the node table. Because the source IP is by
  definition unknown to the member table at that moment, this one request (`POST
  /v1/members {update_addr}`) is authorized by `WhoIs(srcIP).StableID == Node.TSStableID`
  of the claimed node ID, not by IP lookup ([08](08-security.md)).
- **Cluster ID**: 16 random bytes created at `init`, stored in `Meta.ClusterID` and in
  `<data_dir>/cluster_id`. Every Raft stream exchanges it in a 16-byte handshake on dial and
  accept (a wrapper around `raft.StreamLayer`); mismatch closes the connection. Every RPC
  carries it in `X-Tailraid-Cluster`; mismatch is `409`. This prevents two independently
  `init`ed clusters on one tailnet from merging logs.

## Commands

Every command is proposed through `meta.Proposer` and applied by `meta.FSM.Apply`. Every
command carries `Now int64` (unix nanos) from the proposer; the FSM never reads a clock, the
disk, or iteration order of a map when deciding anything (I-4). The FSM keeps
`State.LastNow` and uses `now = max(cmd.Now, State.LastNow)` (then `LastNow = now`) wherever
a timestamp is recorded, so a proposer with a slow clock cannot back-date a tombstone or an
mtime; the FSM clock is monotonic across proposers (`TombstonedAt`, `Mtime`, `Ctime`,
`JoinedAt` all use it). Results are returned in
`Result{Ino, Gen, Victims, SnapID, Err}` with `Err` one of the sentinel errors below — never a Go
panic or a non-deterministic error string.

```go
type Op uint8
const (
    OpSetVolume Op = iota + 1 // {ChunkSize (only at bootstrap), K, M, Now}
    OpMetaSet                 // {ClusterID, JoinToken, CreatedAt}
    OpNodeUpsert              // {Node fields; Idx 0 = allocate}
    OpNodeSetState            // {Idx, State Active|Draining|Removed, Now}
    OpCreate                  // {Parent, Name, Type TFile|TSymlink, Mode, Uid, Gid, Target, Now}
    OpMkdir                   // {Parent, Name, Mode, Uid, Gid, Now}
    OpUnlink                  // {Parent, Name, Now}
    OpRmdir                   // {Parent, Name, Now}
    OpRename                  // {OldParent, OldName, NewParent, NewName, Now}; codec 9 adds {NoReplace bool} (I-224, I-226)
    OpSetAttr                 // {Ino, Mask, Mode, Uid, Gid, Atime, Mtime, Now}
    OpCommit                  // {Ino, BaseVersion, HasTruncate, TruncateTo, NewSize, Writes []ChunkWrite, NewChunks []ChunkDesc, Now}; codec 9 adds {Rewrite u64, RewriteStart bool} (04 §6, I-223)
    OpSetPlacement            // {Hash, Placement []NodeIdx, K, M}   (repair / migration)
    OpGCDelete                // {Before int64} → Result.Victims
    OpSetVolumeMode           // {Auto bool, Now}   (03: automatic redundancy)
    OpTrash                   // {Parent, Name, Recursive bool, Now}: the entry into the trash (codec 4); in codec 8 it is an own cleanup (Own, I-220); codec 9 adds {Expect Ino} (I-225)
    OpRestore                 // {Ino, NewParent (0 = original), NewName ("" = original), Now} (codec 4)
    OpPurge                   // {Ino (0 = every root due), Before int64, Limit uint32, Now} (codec 4)
    OpSetShardSums            // {Hash, Sums [][32]byte}: the data shards' hashes of a chunk committed without them (04 §4, codec 5)
    OpSnapshotBegin           // {Mirror NodeIdx, Name, Auto bool, Now} → Result.SnapID (16 §3, codec 7)
    OpSnapshotEnd             // {ID, OK bool, Root Hash, Files, Bytes, NewBytes uint64, Missing uint32, Now} (codec 7)
    OpSnapshotDelete          // {ID, Done bool, Now}: the mirror host's thinning only, never a user (codec 7)
    OpSnapshotImport          // {Mirror, After SnapID, Name, At, Auto, Root, Files, Bytes, NewBytes, Missing, Now} → Result.SnapID (16 §5, codec 7)
)
const AttrFlags uint8 = 1 << 5  // SetAttr.Mask bit: {Flags uint8} follows Now (codec 4)
const FlagProtected, FlagNSFW uint8 = 1, 2
const FlagLibraryMask uint8 = 0b1100 // the library kind of 18 §9: none, movies, tv, music (codec 6)
type ChunkWrite struct { Idx uint32; Hash Hash }                         // Hash may be HashZero
type ChunkDesc  struct { Hash Hash; Size uint32; K, M uint8; Placement []NodeIdx }
type GCVictim   struct { Hash Hash; Placement []NodeIdx }

var (
    ErrExist, ErrNotExist, ErrNotEmpty, ErrIsDir, ErrNotDir, ErrLoop,
    ErrConflict, ErrUnknownChunk, ErrNeedTailRewrite, ErrBadCommand,
    ErrNoSuchNode, ErrNodeNotEmpty, ErrPolicy,
    ErrProtected, ErrInTrash, ErrCodec, ErrSuperseded error   // appended: wire codes are never renumbered
)
```

### Apply rules

**SetVolume.** `ChunkSize` may only be set when `State.Vol.ChunkSize == 0` (bootstrap);
later commands must repeat the existing value or get `ErrPolicy`. `K >= 1`, `M >= 0`,
`K+M <= 256`. If `K` or `M` changes, `PolicyVersion++`. Existing chunks are untouched; the
repairer migrates them ([06](06-gc-and-repair.md)). This is a day-zero feature: the FSM
accepts it from the first log entry.

**SetVolumeMode.** Sets `Vol.Auto`; `PolicyVersion` is untouched. A node older than the
op rejects the entry (`ErrBadCommand`), so every node is upgraded before the mode is
used (12 §6).

**MetaSet.** Sets `Meta`. `ClusterID` may be set once; later `MetaSet` commands must carry
the same `ClusterID` or the zero value (meaning "unchanged"), else `ErrPolicy`. `token
rotate` is `MetaSet{ClusterID: zero, JoinToken: new}`.

**NodeUpsert.** `Idx == 0` first looks for a live record with the same node ID and updates
it (a join retried after a timeout, possibly at a new leader whose copy has not yet applied
the first upsert, must not create a second member); otherwise it allocates `NextNode`
(never reused) and sets `State = Active`.
Otherwise updates mutable fields (Name, TSStableID, Addr, RaftPort, RPCPort, Capacity, Voter,
Version, S3Endpoint, S3Bucket, S3Prefix, S3Region, S3PathStyle — the `Node` struct of 01) of
an existing node; `ErrNoSuchNode` if absent. `Kind` and `ID` cannot change.

**NodeSetState.** `Active ↔ Draining`, and `Active | Draining → Removed`. `Removed` is
terminal. The FSM refuses the transition to `Removed` with `ErrNodeNotEmpty` while *any*
chunk entry (live or tombstoned) still has a non-zero placement slot on this node (every
slot is a sole copy of its shard index, I-9). The CLI flow is `node drain` → wait for the
repairer to re-place every slot → `node remove`; `--force` first proposes `SetPlacement`
with slot 0 for every remaining slot, making the degradation explicit, then removes. Order
on the leader: `NodeSetState{Removed}` is committed first, then `raft.RemoveServer`, so the
node table never lists a server that Raft has forgotten. A removed node is refused when it
tries to join again until it has been `reset`.

**Create / Mkdir.** `Parent` must exist and be `TDir` (`ErrNotExist`/`ErrNotDir`). `Name`
must be non-empty, not `.` or `..`, contain no `/`, and be ≤ 255 bytes (`ErrBadCommand`).
Existing child ⇒ `ErrExist`. This refusal is what makes an exclusive create exclusive
across gateways: a gateway whose FSM had not yet applied another gateway's `Create` of
the name proposes its own and gets `ErrExist`, and for `O_EXCL` (SMB `FILE_CREATE`, NFS
CREATE `GUARDED`, the browser's `If-None-Match: *`) it answers `EEXIST` rather than
opening what is there (I-222). Allocates `Ino = NextIno++`, `Gen = uint32(logIndex)`, sets
times to `Now`, `Nlink` 1 (dirs: 2, and parent `Nlink++`). Returns `Ino, Gen`.

**Unlink.** Child must exist and not be `TDir` (`ErrIsDir`), and nothing on its parent
chain may be protected (`ErrProtected`). Removes the child entry and the inode. For each
`Chunks[i] != HashZero`: `Chunk.Refs--`; if it reaches 0, set `TombstonedAt = Now` and add
to `Tombs`. Any handle to the inode now fails `FromHandle` (I-3, I-17). The gateways
propose `Unlink` only for the ephemeral names of 06 §1 (upload temps, `._*`,
`.DS_Store`) and while a member is too old for `Trash` (12 §6); every other delete is a
`Trash`.

**Rmdir.** Child must be `TDir` (`ErrNotDir`) with empty `Children` (`ErrNotEmpty`), not
protected and under no protected directory. Removes it; parent `Nlink--`. Like `Unlink`,
proposed only for ephemeral names and in the mixed-version window.

**Rename.** Source must exist. With `NoReplace` (codec 9) an existing destination is
`ErrExist` and nothing changes: the no-replace rename of SMB (I-224) and the browser's
exclusive publish (I-226) are decided by the FSM, not by a lookup on a gateway that may
not have applied the other client's create. Otherwise, if destination exists: file/symlink over file/symlink
replaces it (the destination goes to the trash as by `Trash`, or is unlinked when its
name is ephemeral, then the move); dir over *empty* dir replaces it the same way; dir
over non-empty dir ⇒ `ErrNotEmpty`; type mismatch ⇒ `ErrIsDir` or `ErrNotDir`. Moving a
directory into its own subtree ⇒ `ErrLoop` (walk `NewParent`'s parent chain to root and
reject if it passes through the source). Same path ⇒ no-op success. Protection: a source
that is itself a protected directory ⇒ `ErrProtected`; every protected directory on the
source's parent chain must also be on the destination parent's chain (a move within one
protected subtree is allowed, a move out of it is not); a destination that would be
replaced under a protected directory ⇒ `ErrProtected` (a replace is a removal). Only
`Parent`, `Name`, the two `Children` maps and `Ctime` change; `Ino`, `Gen` and the handle
stay valid (I-16).

**SetAttr.** `Mask` selects Mode, Uid, Gid, Atime, Mtime and, with `AttrFlags`, `Flags`.
`Ctime = Now`. Size is *not* settable here; truncation is a `Commit`. `Flags` is accepted
on a `TDir` only (`ErrNotDir`) and only with the bits of 01 (`ErrBadCommand`); a `Flags`
with the library bits (18 §9) is a codec-6 entry, refused with `ErrCodec` while a live
member reads an older codec, since that member would reject the bits (I-210).

**Trash.** `Parent` must be a live `TDir` holding `Name`; a `TDir` child with children is
refused with `ErrNotEmpty` unless `Recursive` (a POSIX `rmdir` through a mount is not
recursive; a delete from the browser UI is); the child or anything on its parent chain
protected ⇒ `ErrProtected`. Then, with `now` the FSM clock: the child leaves
`Parent.Children`, `DeletedAt = now`, `Trash[Parent]` gains it, `Parent.Nlink--` for a
directory, `Parent.Mtime = Ctime = now`, `Parent.Version++`; the subtree, every `Chunks`
list and every `Refs` are untouched (I-152, L-199), and `Stats.LogicalBytes` moves to
`Stats.TrashBytes` for the files inside. **Folding:** when the child is a directory,
every root in `Trash[child]` (an entry deleted from inside it earlier, as `rm -rf` does
file by file) is put back into `child.Children` under its name, in inode order, with its
own `DeletedAt` cleared; a name already present keeps the newer live entry and the older
root stays a separate root. A fold that put a name back sets `child.Mtime = Ctime = now`
and `child.Version++`, as any change to a name table does: an NFS client that cached the
names' absence after the file-by-file delete revalidates the directory and sees them
again once the folder is restored (07). So a tree removed from a mount lands in the trash as one
entry that restores with one command, and a file deleted from a folder last week comes
back with the folder (13 §4 says so).

**Restore.** `Ino` must be a deleted root (`ErrNotExist` otherwise: an entry under a
deleted directory is restored with it). The target parent (`NewParent`, or the root's
`Parent`) must exist, be a `TDir` and be live, that is neither deleted nor under a deleted
directory (`ErrInTrash`); the name (`NewName`, or the root's `Name`) must be free
(`ErrExist`) and valid (I-66, I-115). No loop check is needed: a live parent cannot lie
under the deleted directory. Apply: the entry enters the parent's `Children`,
`DeletedAt = 0`, it leaves `Trash`, `Parent.Nlink++` for a directory, `Ctime = now`,
`Parent.Version++`, and the stats move back. `(Ino, Gen)` are unchanged, so an NFS handle
that went stale at the delete is valid again (L-200).

**Purge.** With `Ino != 0` the one root (`ErrNotExist` when it is not a root;
`ErrProtected` when it or an ancestor is protected: a user's "delete now" obeys the flag,
the retention purge below does not, since what it reclaims was deleted already); with
`Ino == 0` every root whose `DeletedAt < Before` (strictly, the one aging rule of 06 §2),
in `(DeletedAt, Ino)` order. Each root is deleted post-order, its `Trash` roots counting as
children so a deleted entry inside a deleted directory goes with it, at most `Limit`
inodes per entry (`0` is unbounded): a cut leaves a valid tree and the root stays due for
the next entry, as `GCDelete` drains in batches (I-103). For every `TFile` deleted:
`Refs--` on each `Chunks[i] != HashZero`, a chunk reaching 0 tombstoned at `now` and
added to `Tombs` (L-201, L-202); the same underflow rule as `Unlink`. `Stats.TrashInodes`
and `TrashBytes` shrink accordingly (I-153).

**Protection**, in one place: `walkUp(ino)` follows `Parent` to the root and reports
whether a deleted root was crossed (then the inode is in the trash) and whether a
directory with `FlagProtected` was crossed or is the inode itself. It costs one map
lookup per level, what `pathLen` (I-115) already pays per `Create`. `Unlink`, `Rmdir`,
`Trash`, `Purge{Ino}` and `Rename` consult it as described (I-154); `Create`, `Mkdir`,
`Symlink`, `Commit` and a `Rename` *into* a protected directory are unaffected: a
protected folder accepts new content and refuses to lose any. One `Trash` gets through:
an **own cleanup**, a `Trash` of a file written in codec 8 (`Own`), which the SMB gateway
proposes only for a handle whose CREATE made the file and which the client marked
delete-on-close: a copy that failed removing what it wrote, as Explorer does on Skip or
Cancel (I-220, docs/15 §8). It never removes a directory, and nothing else sets it. In codec 9 a `Trash` may carry
`Expect`, the inode the proposer means: when the name resolves to another inode (a client
renamed the file away and something else took its name) the Trash is `ErrConflict` and
changes nothing, so a delete-on-close never removes a file its handle did not open
(I-225). The
same walk guards the
trash: `Create`, `Mkdir`, `Unlink`, `Rmdir`, `Rename`, `Trash` and a `SetAttr` of
`Flags` refuse a parent (or entry) that lies in the trash with `ErrInTrash`; only
`Commit` writes inside it, for the flusher of a file deleted while dirty (L-203), and
only `Restore` and `Purge` take anything out.

**Commit** — the only command that changes file data. Applied atomically or not at all:

1. `inode` must exist and be `TFile` (`ErrNotExist`/`ErrIsDir`).
   **Rewrites** (codec 9, 04 §6, I-223): a Commit with `RewriteStart` must carry
   `HasTruncate` with `TruncateTo` 0 (`ErrBadCommand`) and skips the `BaseVersion` check
   of step 2: it replaces the whole file whatever was committed since the gateway planned
   it. A Commit with `Rewrite` ≠ 0 and no `RewriteStart` continues the rewrite whose
   start applied at version `Rewrite`; if `inode.RewriteAt != Rewrite` another rewrite
   started since, and the Commit is `ErrSuperseded` (checked before step 2). A Commit with
   neither is a plain write, as before codec 9.
2. `inode.Version != BaseVersion` ⇒ `ErrConflict` (optimistic concurrency; the gateway
   rebases and retries, see 04).
3. Each `NewChunks[j]`: `Hash != HashZero`, `1 <= Size <= ChunkSize`, `K >= 1`,
   `len(Placement) == K+M`, every non-zero `Placement` entry is a distinct existing node
   (`ErrBadCommand` otherwise). If absent from `State.Chunks`, insert with `Refs = 0`. If
   present, the existing entry is kept (dedup); the command's placement is ignored.
4. Each `Writes[i].Hash` must be `HashZero` or present in `State.Chunks` (after step 3),
   else `ErrUnknownChunk`. Tombstoned chunks are valid targets (they get revived).
5. **Truncate first.** If `HasTruncate`: `t = ceil(TruncateTo/ChunkSize)`; `cur =
   old[:min(len(old), t)]`. If `TruncateTo < inode.Size`, `TruncateTo % ChunkSize != 0`
   and chunk `t-1` is neither in `Writes` nor currently `HashZero` ⇒ `ErrNeedTailRewrite`
   (the FSM cannot trim bytes out of a chunk; the flusher always rewrites the straddling
   chunk, see 04). Without `HasTruncate`, `cur = old` and `NewSize >= inode.Size` is
   required (`ErrBadCommand`): a file can only shrink through an explicit truncate.
6. **Then grow.** `NewSize` must not exceed `MaxFileSize(ChunkSize) = ChunkSize × 2^20`
   (`ErrBadCommand`, checked before anything is sized by it: the chunk list of every file
   lives in memory on every node, I-64). `n = ceil(NewSize/ChunkSize)`; `NewSize >= TruncateTo` when
   `HasTruncate`. `newChunks = copy(cur[:min(len(cur), n)])`, extended with `HashZero` to
   length `n`. Every `Writes[i].Idx` must be `< n` (`ErrBadCommand`).
7. **Then write.** Apply `Writes`. For each position whose hash changed (including positions
   dropped by the truncate): old hash (if not `HashZero`) `Refs--` with tombstone on zero;
   new hash (if not `HashZero`) `Refs++` and, if it was tombstoned, `TombstonedAt = 0` and
   remove from `Tombs`. A `Refs--` that would underflow is `ErrBadCommand` (never a panic).
8. `inode.Chunks = newChunks; Size = NewSize; Version++; Mtime = Ctime = now`. A
   `RewriteStart` then sets `inode.RewriteAt = Version` and returns it in
   `Result.Version`; a continuation and a plain Commit leave `RewriteAt` alone, so a third
   writer's in-place WRITE merges into the current rewrite at chunk granularity, as any
   two writers did before codec 9.

**SetPlacement.** Chunk must exist (`ErrUnknownChunk`). Replaces `Placement`, `K`, `M`
(length must equal the new `K+M`; non-zero entries distinct existing nodes). Used by repair
to fill slots, re-place slots off draining/removed nodes, and migrate to a new policy.
`Expect` is the placement the proposer planned from: the command applies only if the
chunk still has it, else `ErrConflict` (I-102, L-197), so a flush filling slots, a repair
pass and a `--force` removal zeroing slots cannot silently undo each other; the loser
re-plans from the current placement or skips the chunk for this pass. The Raft index of
the applied `SetPlacement` (or `GCDelete`) is the fence carried by the shard `DELETE` RPC
([06](06-gc-and-repair.md)).

**GCDelete.** For every chunk in `Tombs` with `TombstonedAt < Before` (and `Refs == 0`,
which `Tombs` guarantees), in hash order and at most `Limit` of them (I-103; `0` is
unbounded): delete it from `Chunks` and `Tombs` and append `GCVictim{Hash, Placement}` to
`Result.Victims`. The leader then deletes the shards and proposes again while a batch
comes back full. Deterministic because `Before` and `Limit` are in the command and the
victim list is emitted in hash order. **The sync hold:** while any catalogue row is
`Syncing`, the FSM uses `min(Before, the oldest syncing row's At)` in place of `Before`,
so no chunk tombstoned after a snapshot began is collected before its copy to the mirror
ends (16 §4, I-181); the bound is state, so every replica applies the same one.

**SnapshotBegin** (16 §3). `Mirror` must be an `Active` `KindMirror` row (`ErrNoSuchNode`);
`Name` 1 to 64 bytes, no `/`, not `.` or `..` (`ErrBadCommand`) and free on that mirror
(`ErrExist`); the mirror must have no `Syncing` row (`ErrExist`). Allocates `ID =
NextSnap++` and records `{ID, Mirror, Name, Index: the command's own index, At: now, Auto,
State: Syncing}`; returns the ID. Nothing else in the state changes. On the mirror's host
the apply also hands a clone of the state at this index to the sync worker; that clone is
local and changes nothing replicated.

**SnapshotEnd.** `ID` must be `Syncing` (`ErrNotExist` otherwise, so a late or repeated end
is harmless). With `OK` the row takes `Root`, `Files`, `Bytes`, `NewBytes` and `Missing`,
and its state becomes `Complete`, or `Incomplete` when `Missing > 0`; without `OK` the row
is removed. Either way the hold it carried ends.

**SnapshotDelete.** `ID` must exist (`ErrNotExist`). Without `Done` a `Complete` or
`Incomplete` row becomes `Deleting` (a `Syncing` row is `ErrExist`: end it first; a
`Deleting` one is unchanged); with `Done` a `Deleting` row is removed (any other is
`ErrExist`). The mirror's disk is cleaned by its host between the two (16 §5), never by
the FSM. The only proposer is the mirror's host, on its tick (thinning and the free-space reserve)
and after its sweep (`Done`): no admin op or web route carries the command, so no
user removes a snapshot (I-183).

**SnapshotImport** (16 §5). A manifest the mirror's host found on its disk with no row
becomes one: `Mirror` must be an `Active` `KindMirror` row (`ErrNoSuchNode`), `Name` valid
and free on that mirror (`ErrBadCommand`, `ErrExist`). Sets `NextSnap = max(NextSnap,
After + 1)`, allocates `ID = NextSnap++` and records `{ID, Mirror, Name, Index: the
command's own index, At, Auto, Root, Files, Bytes, NewBytes, Missing, State: Complete}`
(`Incomplete` when `Missing > 0`); returns the ID. `At` is the manifest's own time, not
the clock's, so the schedule thins an imported snapshot by when it was taken. No hold
begins: the snapshot is already on the disk. The op joined codec 7 before that codec's
first release.

**NodeUpsert of a mirror** (16 §1): a `KindMirror` row must name an `Active` device as its
`Host`, as a disk row does (14 §1), carries no budget and no ports, and is never a
placement, policy or repair candidate; a re-home changes only its `Host`. Removing a mirror
row (`NodeSetState{Removed}`) removes its catalogue rows with it and is refused while one
is `Syncing`.

**Codec versions.** Log entries are written in codec version 1, 2 (`NodeUpsert` with a
disk or host), 3 (`SetPlacement.Expect`, `GCDelete.Limit`), 4 (`Trash`, `Restore`,
`Purge`, `SetAttr` with `Flags`), 5 (`Commit` whose `NewChunks` carry `Sums`,
`SetShardSums`), 6 (`SetAttr` with the library bits of 18 §9) or 7 (`SnapshotBegin`,
`SnapshotEnd`, `SnapshotDelete`, `SnapshotImport`, `NodeUpsert` of a mirror, 16) or 8 (`Trash` with `Own`,
an own cleanup, I-220; the version is the flag, no field is added) or 9 (`Commit` with
`Rewrite`/`RewriteStart`, `Trash` with `Expect`, `Rename` with `NoReplace`; I-223..I-226), snapshots in version 9
(01 §Binary codec: `Inode.RewriteAt`; codec 8 added no state); every reader
decodes every older version. A `Commit` carries `Sums` only while every live active member
reads codec 5 (the flusher asks the prober's `MinCodec`), so a flush never fails on the
gate: in a mixed cluster the chunk is committed without sums and the repair scanner's
backfill records them later (06 §4, I-201). `meta.CodecVersion` is the newest a binary writes; `/health`
reports it and `join` refuses a cluster that reports a newer one (I-105,
[12](12-operations.md) §6). An entry a binary cannot decode still advances its applied
index (so replicas stay aligned) and is counted and logged (I-105). Because a version-4
entry would be exactly that on a member still running codec 3, the leader refuses to
propose one with `ErrCodec` while any live active member reports an older codec in its
`/health` (the prober records it); the gateways fall back to `Unlink`/`Rmdir` for a
delete and to the version-3 replace on `Rename` in that window, so a mixed cluster keeps
working without the trash until every node is updated (I-156, 12 §6). Codec 7 is gated
the same way: `mirror add` and every snapshot command are `ErrCodec` until every live
member reads it, so no node ever applies a hold it cannot see (I-184).

**`SetShardSums`** (04 §4, 06 §4): the chunk must exist with `len(Sums) == K`
hashes, else `ErrBadCommand`; a chunk that already carries sums keeps them when they are
equal and refuses different ones (`ErrConflict`: two leaders cannot disagree about a pure
function of the bytes, so a difference is a bug caught here); refs, placement and
tombstones are untouched, so the command never changes what GC or repair see.

**Idempotency.** Raft may re-deliver entries on restore; `Apply` is pure with respect to the
log so re-applying from a snapshot's index yields the same state (I-5).

## FSM, snapshot, restore

```go
type FSM struct { mu sync.RWMutex; st *State; applied atomic.Uint64; cond *sync.Cond }
func (f *FSM) Apply(l *raft.Log) interface{}            // decode → dispatch → applied=l.Index → cond.Broadcast; returns Result
func (f *FSM) Snapshot() (raft.FSMSnapshot, error)     // RLock; deep-clone State; return &snap{clone}
func (f *FSM) Restore(rc io.ReadCloser) error          // DecodeState; swap under Lock; applied = st.Applied
func (s *snap) Persist(sink raft.SnapshotSink) error   // EncodeState(sink, clone); Cancel on error
func (f *FSM) WaitApplied(ctx context.Context, index uint64) error
```

`hashicorp/raft` guarantees `Apply` never runs concurrently with `Snapshot()` but *does* run
concurrently with `Persist`. Hence `Snapshot()` clones under the read lock (Children maps
deep-copied; `Chunks` slices shared because they are immutable) and `Persist` works on the
clone with no lock. Snapshots are written to `raft.FileSnapshotStore` (retain 2) and fsync
on close. These Raft snapshots (log compaction) are not the volume snapshots of 16,
which are catalogue rows here and trees and chunks on a mirror disk. Log and stable store are `raft-boltdb/v2` in `<data_dir>/raft/raft.db` with
`NoSync = false`.

Raft configuration: `HeartbeatTimeout 2s`, `ElectionTimeout 2s`, `LeaderLeaseTimeout 1s`,
`CommitTimeout 10ms` (how soon followers learn the commit index after an apply with no
successor; a follower gateway's `WaitApplied` bounds on it), `SnapshotInterval 2m`, `SnapshotThreshold 8192`, `TrailingLogs 10240`
(`cluster.Options.Timing` overrides the timeouts; the test harness uses 1 s / 1 s / 500 ms /
10 ms and Raft's in-memory stores through `Options.Stores`, which is how tests get speed
without any switch that skips an fsync).
Tests use `raft.NewInmemTransport`/`NewInmemStore` with 20 ms timeouts.

## Reads

`meta.Store` is the read API of the FSM (`type Store = FSM`); the vfs, gateways, repairer
and CLI reports hold a `*meta.Store`. Every read (`GetInode`, `Lookup`, `Resolve`, `PathOf`,
`ReadDir`, `GetChunk`, `ForEachChunk`, `Nodes`, `Volume`, `Stats`, `Applied`,
`WaitApplied`) is served from the local FSM under `RLock`, returning copies. An inode
copy carries no `Children` map (a directory's name table is read through `Lookup` and
`ReadDir`), so a LOOKUP or GETATTR costs O(1) however large the directory. The trash is
invisible to these reads: `Resolve`, `Lookup` and `ReadDir` start from live directories
and never meet a deleted root, and `GetInode` fails with `ErrNotExist` for a deleted root
and everything under it, so a gateway's handle to a deleted file is stale (I-17) and
valid again after a restore (I-155). The trash has reads of its own: `GetInodeAny` (the
raw lookup the flusher and an open file use, so a dirty file that was deleted still
commits into its trash entry), `TrashOf(parent)` (the roots under a directory, newest
first then by inode), `Trash()` (every root), `TrashDue(before)` (how many the retention
purge would take) and `ResolveFrom(root, parts)` (a path inside a deleted root), which
the browser UI and `tailraid trash` use. Followers may therefore serve slightly stale
metadata; this is accepted NFS behaviour and lets a disconnected laptop browse the tree.
A gateway that just proposed a command calls `WaitApplied(index)` before answering, which
gives read-your-writes on the issuing node.

The **stale window** this creates is one replication round in normal operation and the
whole partition on a minority side: a gateway that has not yet applied another gateway's
unlink, rename or create resolves the *old* name table. A client there may open an inode
that is already unlinked (its later WRITEs are accepted into the WAL and dropped when the
Commit answers `ErrNotExist`, docs/04), see `ENOENT` for a file that exists elsewhere, or
create a name that the leader then reports as `ErrExist` (for an exclusive create the
client sees `EEXIST`, for any other the gateway opens the winner's file, I-222). These are the NFS close-to-open
anomalies of any multi-server filesystem with asynchronous metadata; what never happens is
a committed byte changing because of them (I-12, I-35). The model checker encodes exactly
this rule: after a remove or rename nobody writes the affected names until every node has
caught up, and a definite answer from a stale gateway counts as "nothing happened".

Health, computed locally:

| `Health` | Condition | Gateway behaviour |
|---|---|---|
| `Healthy` | leader known, last contact < 30 s | normal |
| `Stale` | no leader contact for ≥ 30 s | reads from local FSM; metadata mutations (create, mkdir, unlink, rename, setattr) return `EROFS`, which reaches the wire as `NFS3ERR_ACCES` (create, mkdir, symlink, setattr) or `NFS3ERR_IO` (remove, rmdir, rename): go-nfs maps a filesystem error per operation and has no `ROFS` mapping (07); before the threshold a failed proposal is retried with backoff, since a leader change takes seconds; data WRITEs to existing files are still accepted into the WAL until the dirty caps (04), then `NFS3ERR_IO` |
| `NoLeader` | Raft has no leader (an election, or quorum lost) | as `Stale` once the threshold has passed since the last leader contact (for a node that was the leader, which Raft dates no contact for, since it first saw no leader); before that proposals are retried with backoff, so a two-second election costs a client a wait, not an error; `status` reports which voters are unreachable |

## Proposing and leader forwarding

`hashicorp/raft` only accepts `Apply` on the leader. `cluster.Cluster.Apply` applies locally
when leader, otherwise forwards the encoded command to the current leader with
`POST /v1/apply` over the node RPC. Semantics are exact:

- `ErrNotLeader` (the target was not leader when it received the command): the command was
  **not** applied; the caller retries against the new leader (bounded, with backoff).
- `ErrLeadershipLost`, timeout, a cancelled context while the future is pending, a
  transport error, or the leader's `500` after it handed the command to Raft: the outcome
  is **unknown**. Every such failure is reported as `meta.ErrUnknownOutcome` (wrapping the
  cause); the forwarding layer never retries it by itself (I-29) and the leader's `apply`
  handler never disguises it as "not the leader" (I-95). A context that is already expired
  is refused before dispatch, since Raft disarms its timer for a zero timeout.
- *Callers* resolve an unknown outcome instead of retrying blindly (I-95): the gateway
  proposes the command once more and reads the second answer knowing a first attempt may
  have landed. `Commit` carries `BaseVersion`, so the repeat either applies or returns
  `ErrConflict`, after which the flusher reloads the inode and re-plans. `Create` and
  `Mkdir` may hit `ErrExist`, which the gateway maps to success when the existing child
  matches; a `Symlink` only when the existing link has the same target. `Unlink`, `Rmdir`
  and `Rename` take `ErrNotExist` after an unknown outcome as the first attempt having
  landed. `SetAttr` sets absolute values and is simply repeated. A repeat whose outcome is
  unknown again is proposed again, with the backoff below, each answer read the same way,
  until the gateway is stale (`EIO` then): a leader killed mid-failover leaves a forward
  in flight and a second one aimed at its address before the new leader is known, and a
  client's CREATE must ride that out (soak phase 22 found it failing). Commands whose first proposal was *not* applied (`ErrNotLeader`,
  `ErrNoLeader`, or a forward whose request never left because the leader's address
  refused the dial or a fault rule dropped it) are retried with backoff until the gateway
  is stale: the leader is out of reach, which an election or a heal mends within seconds.
- A leader with no quorum eventually steps down; proposers see `ErrNoLeader` and the flusher
  backs off (data stays in the WAL, see 04).

## Membership

**Bootstrap** (`tailraid init`): refuses if `<data_dir>/raft` has state. Generates
`node_id`, `cluster_id`; `raft.BootstrapCluster` with self as the only voter; applies
`MetaSet`, `SetVolume{ChunkSize, 1, 0}`, `SetVolumeMode{Auto: true}`, `NodeUpsert(self)`
(no budget: a device stores nothing, 14 §1) and a `NodeUpsert` per configured disk with
its derived budget (01). The node is named after the host; `--name`, `--chunk` and
`--token` are the only options.

**Join** (`tailraid join <host> <token> [--voter]`): refuses if local Raft state exists.
Generates `node_id`; `POST /v1/join` with `{token, node_id, name, ts_stable_id, addr, ports,
voter, version}`. The receiver requires `WhoIs(srcIP).StableID == ts_stable_id`
and `token == Meta.JoinToken`, then forwards to the leader, which calls
`raft.AddNonvoter` or `AddVoter` and proposes `NodeUpsert`. A voter just added and not
yet caught up can cost the leader its lease while that proposal is in flight; the
outcome is then unknown (I-95) and the reply is `503`, which the joiner retries under
the same node ID: a re-join keeps the row and index whichever way the proposal went
(I-94). The joiner then starts `serve`
and receives the state by snapshot, after registering its configured disks through the
leader's `add_disk` member op (14 §2, I-121).

**Disks.** `node add-disk` (14 §2) proposes `NodeUpsert{Kind: KindDisk, Host}` through
the leader; a disk row is a placement target, never a Raft member, and `init` registers
the disks already in the config in its bootstrap.

**Voters.** Only devices configured `voter: true` (and the `init` node) are Raft voters.
Non-voters replicate the log, serve reads and act as gateways and storage, but do not count
toward quorum, so sleeping laptops never stall the cluster. `status` warns when the voter
count is 1 or even.

**Lifecycle commands** (all day-zero): `node promote|demote` (`AddVoter`, or `DemoteVoter` for a server that votes:
raft's `AddNonvoter` on a voter changes only its address, so a demote that used it left
the device voting while its row said otherwise; a leader demoting itself records the row
first, then steps down to follower once the change commits, never shutting Raft down,
I-145), a disk's budget (`set_capacity` → `NodeUpsert`, proposed by the disk's host as
its free space changes, 01; refused for a device row, I-118), `node drain` (`NodeSetState Draining`:
excluded from new placement; repairer re-places its slots), `node remove`
(`NodeSetState Removed`, then `raft.RemoveServer`; a removed row stays in the table for its history, so a device reset and joined again has two rows with one name: a name on the command line names the row that is not removed, and the leader refuses every op but `remove` on a removed row, since `promote` or `update_addr` there would put a dead ID back into the Raft configuration, I-136), `set_version` (`NodeUpsert` with the
version `serve` runs: proposed by the node at start, through the leader, whenever its row
names another, so `node ls` shows what each node runs rather than what it joined with,
I-133), `node add-s3` (`NodeUpsert` of a `KindS3` row by the leader's `add_s3`, refused for a name or bucket already registered, I-137, 10 §4.1), `volume set-redundancy` (`SetVolume`
after `SetVolumeMode{false}`; `--auto` is `SetVolumeMode{true}`),
`tailraid reset --yes` (local only: deletes `<data_dir>/raft` and `node_id`/`cluster_id` so
the device can `join` afresh; never touches `shards/`, which the sweep reconciles later; it
refuses while `serve` runs and while `staging/` holds unflushed writes, unless
`--discard-dirty` accepts losing them).

Every member op runs on the leader behind a Raft barrier, so it reads the node table as
of every entry the leader has committed. A leader elected before its FSM replayed its
log (the first seconds after a restart from a snapshot older than the log) would
otherwise read the snapshot's copy of the row, and since `NodeUpsert` replaces the whole
row, the `set_version` a restarting node reports at that moment reverted the row's
budget, address or vote to the snapshot's (CI, 2026-10-11; I-177).

## Partitions and quorum

Raft guarantees that only a partition containing a majority of voters elects a leader and
commits entries (I-6). The minority side's gateways move to `Stale`/`NoLeader`: reads
continue from the local FSM and from any reachable shard holders; metadata mutations return
`EROFS` (`NFS3ERR_ACCES`/`NFS3ERR_IO` on the wire, 07); data WRITEs are still accepted into the WAL until the dirty caps, and
everything in the WAL sits there until the flusher can propose again.
When the partition heals, the minority's Raft log is reconciled by Raft itself; our FSM has
no divergent state to merge because it never applied uncommitted entries.

## Invariants

- **I-4** — The FSM is deterministic: applying the same sequence of log entries to any number
  of fresh FSMs yields byte-identical `EncodeState` output; `Apply` never consults a clock,
  the filesystem, randomness, or Go map iteration order.
- **I-5** — Snapshot and restore are equivalent to replay: for any prefix of the log,
  `Restore(Snapshot(prefix))` followed by applying the remaining entries yields the same
  state as applying the whole log.
- **I-6** — Under any network partition, metadata operations are committed on at most one
  side (the one with a voter majority); a gateway on the minority side never reports a
  metadata write as successful.
- **I-26** — Every failure of a command is reported as one of the sentinel errors in
  `Result.Err`, and a failed command leaves `State` unchanged.
- **I-27** — Reference counts are exact: for every chunk, `Refs` equals the number of
  `(inode, index)` positions whose hash is that chunk, and `Tombs` is exactly the set of
  chunks with `Refs == 0`.
- **I-28** — A `Commit` whose `BaseVersion` does not match the inode's current `Version` is
  rejected without side effects, so two gateways cannot interleave partial updates to one
  file.
- **I-62** — A node ID has at most one record that is not `Removed` in the node table: a `NodeUpsert` with `Idx == 0` for a known ID updates that record and never allocates a new index, so a retried join is idempotent whichever leader serves it.
- **I-64** — No command makes a file larger than `MaxFileSize(ChunkSize)` (`ChunkSize × 2^20` chunks): a `Commit` beyond it is `ErrBadCommand` before anything is allocated from its size, the gateway refuses the WRITE or SETATTR with `EFBIG` first, and `Apply` never panics on a decodable command.
- **I-29** — The leader-forwarding layer never applies a command twice: a forwarded command
  is handed to Raft at most once per proposal, and the forwarding layer itself retries only
  after an explicit `ErrNotLeader`. (Callers may retry idempotent commands as described
  above; that is outside this invariant.)
- **I-96** — `node_id` and `cluster_id` are written atomically (temp file, fsync, rename,
  directory fsync), so a crash leaves the old file or the new one; a read error other
  than "not found" on `node_id` is an error, never a fresh identity.
- **I-99** — A node that fails to open or bootstrap releases its Raft listener and its
  stores before returning the error, so the same data directory and address can be
  opened again at once.
- **I-102** — A `SetPlacement` whose `Expect` differs from the chunk's current placement
  is rejected with `ErrConflict` and changes nothing, so two proposers that planned from
  different placements never overwrite each other's slots unnoticed.
- **I-103** — A `GCDelete` removes at most `Limit` entries, the lowest hashes first, and
  the GC worker proposes again while a batch comes back full, so a backlog of any size
  drains in bounded applies and every replica removes the same entries per entry.
- **I-104** — A snapshot is a deep copy of the mutable state taken under the read lock,
  and chunk lists are immutable (a `Commit` installs a new list, never rewrites one): a
  `Commit` applied after the snapshot never changes it, and a snapshot is written and read
  as a stream, never held whole in memory a second time.
- **I-105** — A log entry this binary cannot decode is counted (`status` shows
  `incompatible_entries` and a warning) and logged, while the applied index advances as on
  every replica; `/health` reports `meta.CodecVersion`, and `join` refuses a cluster whose
  node reports a newer one.
- **I-95** — A proposal that may have been applied (leadership lost, timeout, cancelled
  while pending, a transport error after the request was sent, or the leader's `500`) is reported as
  `meta.ErrUnknownOutcome` and never as "not the leader"; the gateway re-proposes it (with
  backoff, until stale) and resolves the answer per command (`ErrExist`/`ErrNotExist` after an unknown
  outcome count as the first attempt having landed), so an acknowledged mutation is never
  reported as failed and a failed one is never silently repeated. No proposal outlives
  its node: `Close` refuses new proposals and, on a leader, drains what Raft committed
  into the FSM (a barrier) before Raft stops, since raft's FSM loop drops its queue on
  shutdown and a future for a committed-but-unapplied entry would otherwise never
  resolve; every waiter returns and no goroutine is left behind.
- **I-133** — A node's row carries the version its `serve` runs: at start a node whose
  row names another version proposes `set_version` through the leader, retrying until
  one accepts it, and a row that already matches is never rewritten; `set_version`
  changes nothing but the version.
- **I-145** — After `set_voter` a device's Raft suffrage equals its row's `Voter`: a
  demote takes the vote with `DemoteVoter`, and a leader that demotes itself records the
  row before the change and keeps running Raft as a follower after it.
- **I-177** — A member op on the leader reads the node table only after a Raft barrier
  has applied every entry the leader had committed: a leader whose FSM still lags its
  log never rewrites a row from a stale copy, so a `set_version` sent to a freshly
  elected leader keeps every other field of the row.
- **I-136** — A member op other than `remove` on a removed row is refused before it touches the Raft configuration or the table, and a node name given to the CLI resolves to a row that is not removed when one has that name.
- **I-152** — `Trash` moves an entry out of its parent's name table and nothing else:
  the inode, its subtree, every chunk list and every `Refs` are unchanged, `DeletedAt`
  is the FSM clock, and the entry is indexed under the parent it came from; a directory
  being trashed takes back the roots deleted from inside it earlier (folding), so a tree
  removed entry by entry is one root; and `Restore` puts a root back with the same
  `(Ino, Gen)`, under its original name or the name and live parent asked for, refusing a
  taken name with `ErrExist` and a parent in the trash with `ErrInTrash`.
- **I-153** — `Purge` is deterministic across replicas and exact: with `Ino` it takes
  that root, otherwise every root with `DeletedAt < Before` in `(DeletedAt, Ino)` order,
  deleting post-order with nested roots as children and at most `Limit` inodes per entry
  while leaving a valid tree; it decrements `Refs` once per `(file, index)` it deletes,
  tombstoning chunks that reach zero at the FSM clock, never underflowing; and after it
  `Trash`, `TrashInodes` and `TrashBytes` equal a recount.
- **I-154** — A directory with `FlagProtected` and everything under it cannot leave the
  namespace: `Unlink`, `Rmdir`, `Trash` and a user's `Purge` of such an entry, a `Rename`
  of the protected directory itself, a `Rename` that moves an entry out of a protected
  subtree, and a `Rename` that would replace an entry under one are refused with
  `ErrProtected` before anything changes, the own cleanup of I-220 alone excepted;
  creating, writing and moving into a protected directory succeed; `Flags` are accepted
  on directories only.
- **I-222** — An exclusive create of one name succeeds exactly once across the cluster:
  `vfs.OpenFile` with `O_CREATE|O_EXCL` answers `EEXIST` when the name resolves locally
  and also when its `Create` proposal returns `ErrExist`, never opening or truncating the
  file another gateway created; SMB `FILE_CREATE`, NFS CREATE `GUARDED` and the browser's
  `If-None-Match: *` publish all go through it, so two clients creating one name on two
  gateways, or on one gateway over two connections, see one success and one
  `EEXIST`/`STATUS_OBJECT_NAME_COLLISION`/`NFS3ERR_EXIST`/412, and the file holds the
  winner's bytes; a refused proposal is waited for at its index, so the winner's file
  resolves on the losing gateway. After an unknown outcome (I-95) the repeat's
  `ErrExist` opens the file as the gateway's own only when it is an empty file created
  between the command's time and the answer, the one window in which two clients can
  both be told "created".
- **I-220** — A `Trash` with `Own` (codec 8) of a file under a protected directory moves it
  into the trash like any delete; with `Own` a directory is still refused with
  `ErrProtected`, and without it everything under a protected directory is (I-154). The
  leader refuses the codec-8 entry while a live member reads less (I-156), and the
  gateway then answers the plain refusal. The SMB gateway proposes it for exactly the
  handle whose CREATE created the file and which is marked delete-on-close, at SET_INFO
  disposition and at CLOSE; a delete through another handle, NFS, or the browser is a
  plain `Trash`.
- **I-155** — The trash is invisible to the gateways and readable by the browser UI: a
  deleted root and everything under it fail `GetInode`, `Lookup`, `Resolve` and `ReadDir`
  (a handle is stale, a name is free, a listing omits it) and are reachable through
  `GetInodeAny`, `TrashOf`, `Trash` and `ResolveFrom`; a dirty file that is deleted keeps
  its WAL and its flush commits into the trash entry, so a restore shows every
  acknowledged byte, and a purge before that flush drops the WAL as an unlink does.

## Tests

| Invariant | Tests |
|---|---|
| I-4 | `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`, `TestFSM_ApplySnapshotRestoreEquivalence`, `TestFSM_DeterministicAcrossReplicas`, `TestModelCheck_Long` |
| I-5 | `TestCluster_RejoinAfterLongAbsence_SnapshotInstall`, `TestFSM_ApplySnapshotRestoreEquivalence`, `TestSingleNode_BootstrapProposeRestart` |
| I-6 | `TestCluster_SplitBrain_OnlyMajorityCommits` |
| I-26 | `FuzzDecodeCommand`, `TestEncodeCommand_BadCommandIsError`, `TestErrorCode_RoundTrip`, `TestFSM_BadCommandLeavesStateUnchanged`, `TestFSM_Commit_NewSizeBounds`, `TestFSM_Commit_RejectsImplicitShrink`, `TestFSM_Create_NameBoundaries`, `TestFSM_NodeRemove_RefusedWhileHoldingSlots`, `TestVFS_RenameIntoOwnSubtree_NoSideEffects` |
| I-27 | `TestCheckInvariants_DetectsEachViolation`, `TestFSM_Commit_NewFileAndOverwrite`, `TestFSM_Commit_SameHashTwiceInOneFile_RefsPerIndex`, `TestFSM_Revive` |
| I-28 | `TestFSM_Commit_RejectsStaleBaseVersion` |
| I-62 | `TestFSM_NodeUpsert_IdempotentByID` |
| I-95 | `TestVFS_UnknownOutcome_ResolvesPerCommand`, `TestApply_UnknownOutcomeIs500`, `TestApplyLocal_ExpiredAndCancelledContext`, `TestCluster_CloseWhileApplying_EveryWaiterResolves`, `TestJoin_UnknownOutcomeIsRetryable` |
| I-96 | `TestLoadOrCreateNodeID_ReadErrorIsNotANewIdentity` |
| I-102 | `TestFSM_SetPlacement_ExpectConflict`, `TestEncodeCommand_Golden` |
| I-103 | `TestFSM_GCDelete_LimitBoundary`, `TestGC_DrainsBacklogInBatches` |
| I-104 | `TestFSM_SnapshotUnaffectedByLaterCommit`, `TestDecodeState_StreamsFromAnyReader` |
| I-105 | `TestFSM_IncompatibleEntryCounted`, `TestCodecCompatible`, `TestMembersBadSlotHealthStatus` |
| I-99 | `TestOpenFresh_StartFailureFreesPortAndStores` |
| I-64 | `TestChunkCount_NoOverflow`, `TestFSM_Commit_NewSizeBounds`, `TestVFS_SizeBeyondMax_EFBIG` |
| I-29 | `TestApply_ForwardAndRedirect`, `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`, `TestFSM_WaitApplied_ReturnsOnClose`, `TestModelCheck_Long` |
| I-133 | `TestMember_SetVersion`, `TestReportVersion_UpdatesOwnRow` |
| I-145 | `TestCluster_DemotePromote_RaftFollowsRow` |
| I-177 | `TestMemberOp_ReadsAppliedRow` |
| I-136 | `TestMember_RemovedRowRefusesOps`, `TestFindNode_PrefersLiveRow` |
| I-152 | `TestFSM_Trash_Table`, `TestFSM_Trash_FoldsEarlierRoots`, `TestFSM_Restore_Table`, `TestFSM_DeterministicAcrossReplicas`, `TestCluster_TrashRestoreAcrossNodes`, `TestModelCheck_Long` |
| I-153 | `TestFSM_Purge_Table`, `TestFSM_Purge_Boundary`, `TestFSM_Purge_CascadesNestedRoots`, `TestFSM_TrashStats_MatchComputed`, `TestCheckInvariants_DetectsEachViolation`, `TestFSM_ApplySnapshotRestoreEquivalence`, `TestFSM_DeterministicAcrossReplicas`, `TestModelCheck_Long` |
| I-220 | `TestFSM_Protected_OwnCleanup_Table`, `TestEncodeCommand_OwnTrashIsCodec8`, `TestSMB_FailedCopyCleanup_UnderProtected`, `TestSMB_DeleteExistingUnderProtected_StillRefused`, `TestDeleteOnClose_OwnCreationIsOwnCleanup` |
| I-154 | `TestFSM_Protected_Table`, `TestFSM_SetAttr_Flags`, `TestVFS_Protected_EPERM`, `TestNFS_Protected_IsACCES`, `TestSMB_Protected_AccessDenied`, `TestDeleteOnClose_AsksTheBackend` |
| I-155 | `TestStore_TrashReadAPI`, `TestFSM_Trash_Table`, `TestNFS_TrashedIsGoneFromShare`, `TestNFS_TrashedHandleStale_RestoredValid`, `TestSMB_DeletedIsGoneFromShare`, `TestVFS_Remove_TrashKeepsWAL_FlushCommits`, `TestCluster_TrashRestoreAcrossNodes`, `TestModelCheck_Long` |
| I-222 | `TestVFS_ExclCreate_LostRace_EEXIST`, `TestVFS_ExclCreate_UnknownOutcome_OwnFileOpens`, `TestCluster_ExclCreate_TwoGateways_OneWins`, `TestSMB_FileCreate_RacingBackend_Collision`, `TestNFS_CreateGuarded_RacingBackend_Exist`; planned: TestSoak_MultiUserAgainstHarness |
