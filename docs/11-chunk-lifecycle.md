# 11 — Chunk and shard lifecycle

## Purpose and how to read this document

This is the authoritative model of everything that can happen to a chunk (one 1 MiB logical
block of a file) and to a shard (one erasure-coded piece of a chunk stored on one node) from
the first dirty byte to final deletion. It defines the states, every event that can act on
them, the complete table of reachable transitions with their observable outcome and the test
that proves it, and the table of transitions that must never happen. Every cell has a stable
identifier `L-n`. Tests cite the cells they exercise (`// Lifecycle: L-12, L-40`), the
`scripts/check-invariants.sh` script fails if a cell has no test, and every bug fix adds a
regression test citing the cell it belongs to (`// Regression: #<issue> L-n`). Read §2–§3 for
the vocabulary, §4 for the events, §5–§6 for the matrix, §7 for how it is executed, §8 for the
invariants this document owns. Field names refer to `docs/01-data-model.md`; the write path
is `docs/04`; repair and GC are `docs/06`.

## 2. Chunk state machine

A chunk is identified by its SHA-256 `Hash` once assembled; before that it is identified by
`(ino, chunkIdx)` on the gateway that holds the dirty bytes. States are mutually exclusive
and derivable from (gateway Overlay/WAL, FSM `State`, node shard stores, node liveness):

| State | Definition (what makes a chunk be in this state) |
|---|---|
| **Dirty** | The gateway `Overlay` has at least one dirty extent in `dirtyFile.ext` intersecting `[idx·CS, (idx+1)·CS)` for inode `ino`, backed by fsynced records in `staging/<ino>.wal`. No hash yet. Not visible to other gateways. |
| **Assembled** | `Flusher.FlushInode` has produced the full logical chunk bytes (WAL extents over base) and computed `Hash` (or detected all-zero → `HashZero`). Transient, in flusher memory only. |
| **DedupHit** | `Hash` of an Assembled chunk exists in `State.Chunks` (live or tombstoned). No shards are uploaded. Transient; proceeds to Commit. |
| **Uploading** | Shards encoded with chosen `(k,m)`, `Place` computed, PUTs in flight. `State.Chunks` has no entry for `Hash`. Shards already landed are Orphan-candidates until Commit. |
| **Placed(n of k+m)** | PUT phase finished with `n ≥ k` acks within the deadline; `NewChunks` entry prepared with `Placement` slots (0 for failed). Not yet proposed or not yet applied. |
| **Committed** | `State.Chunks[Hash]` exists, `Refs ≥ 1`, `TombstonedAt == 0`, every slot non-zero, every slot's node is `Active` and live (within `fill_after`), and `(K,M) == Vol.(K,M)`. Steady state. |
| **Degraded** | `State.Chunks[Hash]` exists with `Refs ≥ 1` and at least one of: a slot is 0; a slot's node is `Draining`/`Removed`; a slot's node has been dead longer than `fill_after`; a slot was reported bad by a reader (`POST /v1/bad-slot`) or its shard is Quarantined; `(K,M) ≠ Vol.(K,M)` (migration pending). Readable iff ≥ k slots reachable and intact. Counted in `status` as degraded. |
| **Repairing** | The leader's repairer has dequeued the chunk: reading k shards, encoding, PUTting replacement slots; ends with `SetPlacement`. Transient; the FSM still shows Degraded until `SetPlacement` applies. |
| **Tombstoned** | `Refs == 0`, `TombstonedAt > 0`, present in `State.Tombs`. Shards remain on their nodes. Readable by nothing (no inode references it). |
| **Revived** | A `Commit` referenced a Tombstoned hash: `Refs` became ≥ 1 and `TombstonedAt` reset to 0 in the same apply. Transient marker; the chunk is then Committed or Degraded by the rules above. |
| **Collected** | `GCDelete{Before}` removed the entry from `State.Chunks` (returned in `Victims`); the leader is deleting shards. The hash no longer exists in metadata. Any later reference must raise `ErrUnknownChunk`. |
| **Orphan** | A node holds a shard whose `Hash` has no `State.Chunks` entry (upload never committed, Collected but DELETE failed), whose `(k,m)` in the key differs from the entry's, or whose placement no longer names this node (Relocated). Chunk-level view of orphan shards. |
| **Swept** | Terminal: the node `Sweep` deleted Orphan shards older than the grace period. |

Two readings of "degraded" come out of the same derivation (`meta.ChunkState`): `status`,
`check` and `chunk describe` call a chunk *degraded* as soon as fewer than `k+m` copies are
reachable, because an operator wants to know at once; the **Degraded** state above, which
the repair scanner acts on, additionally requires a bad slot (a dead holder only after
`fill_after`), and `status` reports that count as `repair_pending`.

```
                 WRITE/truncate                flush trigger
   (nothing) ────────────────▶ Dirty ────────────────────▶ Assembled
                                 ▲  │ unlink before flush      │
                                 │  └─────────▶ (dropped)      │ hash
             conflict rebase /   │                             ▼
             <k acks / no quorum │          exists?  ┌─── DedupHit ──┐
                                 │                   │               │ Commit
                                 │        no         ▼               ▼
                                 └──────────── Uploading ──▶ Placed ──▶ Committed ◀──┐
                                                  │  acks<k    │  Commit        │   │ repair done
                                                  ▼            │  ErrNotExist   │   │ (SetPlacement)
                                               Orphan ◀────────┘                ▼   │
                                                  │ Sweep (old)        slot0/dead/drain/policy
                                                  ▼                             │
                                                Swept                       Degraded ──▶ Repairing
                                                  ▲                             │ last ref
                                                  │ DELETE fails                ▼
                       Revived ◀─ Commit refs ─ Tombstoned ──GCDelete(after grace)──▶ Collected
                          │                                                     │ DELETE ok
                          └──▶ Committed/Degraded                               ▼
                                                                             (gone)
```

## 3. Shard state machine

A shard is `(Hash, idx)` on one node, stored at `shards/<hh>/<hex>.<idx>` with the `TRS1`
header (`magic | payloadLen u32 | sha256(payload) | payload`), or as an S3 object with the
same key under the node's prefix.

| State | Definition |
|---|---|
| **Temp** | File exists as `shards/<hh>/.tmp-<rand>` in the same directory, being written. Never served; `Store.Get` cannot see it. |
| **Durable** | `write → f.Sync() → Close → Rename` completed; file visible under its final name with a complete header. Served to readers. |
| **Verified** | Durable and the last `Get`/`Open`/scrub recomputed the payload hash and it matched the header (observational substate of Durable, recorded in the event ring). |
| **Corrupt** | Header magic/length/hash does not match the payload (bit flip, truncation, zero-fill) — detected only on read or scrub, never assumed. |
| **Quarantined** | A Corrupt file has been moved by the store to `quarantine/<key>.<unixnano>` (`ErrCorrupt` returned; `shards_corrupt` metric incremented; never served). The chunk's placement still names this slot until repair. |
| **Repaired** | The repairer wrote a new Durable shard for the same key (same node, or a different node via Relocated). |
| **Unreachable** | The holding node is not live (no successful `/health` within 15 s, or HEAD-bucket failing for an S3 node). Shard bytes may be intact; the reader skips the slot. |
| **Reachable** | The holding node is live. Default. |
| **Relocated** | `SetPlacement` moved the slot to another node. The old copy, if still on disk, is now an Orphan shard (placement no longer names its node) and will be Swept. |
| **Deleted** | Removed by an explicit `DELETE /v1/shards/{key}` (GC after Collected, or repairer after a committed relocation/migration). |
| **Swept** | Removed by the node's `Sweep` (older than grace and not placed on this node in the replicated chunk table). |

## 4. Event catalogue

Each event is `E-n`. Events are the only way state changes; the transition table in §5 uses
these numbers.

**Client operations (via NFS or S3 gateway)**
- E-1 WRITE at offset 0 of a new/empty file
- E-2 WRITE inside an existing committed chunk (read-modify-write at flush)
- E-3 WRITE at EOF (append)
- E-4 WRITE past EOF (hole created)
- E-5 WRITE spanning a chunk boundary
- E-6 WRITE overlapping an earlier dirty extent of the same chunk
- E-7 WRITE of exactly one aligned full chunk
- E-8 Truncate down mid-chunk
- E-9 Truncate down to a chunk boundary
- E-10 Truncate to 0
- E-11 Truncate up
- E-12 Truncate down below existing dirty extents
- E-13 Unlink (last link) of a file
- E-14 Rename (same name space, ino unchanged)
- E-15 Rename over an existing file
- E-16 Create/write a second file with identical content
- E-17 Concurrent writers on two gateways, different chunks of one file
- E-18 Concurrent writers on two gateways, same chunk
- E-19 Dirty bytes exceed `MaxDirty`
- E-20 S3 PUT (streamed), S3 multipart Complete (chunk-aligned splice)

**Flush**
- E-21 Idle trigger (2 s), E-22 per-inode dirty ≥ 32 MiB, E-23 total dirty ≥ 256 MiB, E-24 shutdown `FlushAll`, E-121 per-inode WAL ≥ 128 MiB
- E-25 Assemble needs base chunk bytes (RMW)
- E-26 Assembled bytes are all zero
- E-27 Hash exists live (dedup), E-28 hash exists tombstoned (revive)
- E-29 Chunk ≤ 64 KiB (k=1 replication), E-30 chunk > 64 KiB (policy (K,M))
- E-31 Volume policy changed since last flush
- E-32 Tail chunk must be rewritten after truncate

**Transport (PUT/GET/DELETE of shards)**
- E-33 PUT acknowledged 200/201, E-34 PUT connection failure, E-35 PUT timeout, E-36 PUT 507 ENOSPC,
  E-37 PUT 400 hash mismatch, E-38 PUT 403 not a member, E-39 PUT 409 cluster-id mismatch,
  E-40 PUT to slow S3 node past the 10 s deadline, E-41 duplicate PUT of an existing key,
  E-42 self is in placement (local store write), E-43 GET 404, E-44 DELETE for a key still placed on the node

**Consensus**
- E-45 Commit applied OK, E-46 ErrConflict (BaseVersion stale), E-47 ErrUnknownChunk, E-48 ErrNotExist,
  E-49 ErrNeedTailRewrite, E-50 ErrNotLeader, E-51 ErrLeadershipLost / apply timeout,
  E-52 No quorum (Health NoLeader/Stale), E-53 Follower restores a snapshot, E-54 Leader change,
  E-55 WaitApplied returns on the issuing gateway

**Reads**
- E-56 Read with all slots reachable, E-57 read with m slots unreachable, E-58 read with m+1 unreachable,
  E-59 cache hit, E-60 cache entry corrupt, E-61 one fetched shard ErrCorrupt/404,
  E-62 decoded chunk hash ≠ Hash, E-63 read beyond the chunk's stored Size (zero-fill),
  E-64 read of a HashZero index, E-65 read on the issuing gateway while Dirty,
  E-66 read on another gateway before its FSM applied the Commit

**GC**
- E-67 Last reference dropped (Refs→0), E-68 GCDelete before grace, E-69 GCDelete after grace,
  E-70 shard DELETE RPC fails after Collected, E-71 Sweep finds a young Orphan shard (< grace),
  E-72 Sweep finds an old Orphan shard, E-73 Sweep finds a shard whose placement names another node,
  E-74 Sweep finds a shard placed on self, E-75 Sweep finds a shard of a Tombstoned chunk

**Membership and liveness**
- E-76 Node dead < fill_after, E-77 node dead > fill_after, E-78 node dead > evict_after,
  E-79 node returns, E-80 `node drain`, E-81 `node remove`, E-82 `node remove --force`,
  E-83 `join` new node, E-84 `set-capacity`, E-85 Tailscale IP change,
  E-86 `volume set-redundancy` (k+m up or down), E-87 fewer live nodes than k+m,
  E-88 S3 node slow/unreachable, E-89 repair PUT fails, E-90 node returns mid-repair with its old shard

**Integrity**
- E-91 1..m shards corrupt, E-92 m+1 shards corrupt, E-93 header mismatch, E-94 truncated shard file,
  E-95 scrub verifies OK, E-96 scrub detects corruption, E-97 shard file missing (deleted outside tailraid),
  E-98 S3 object missing/HEAD 404, E-99 node capacity exceeded

**Crash points** (`cluster.Hooks` names in parentheses; `C1..C7` in `docs/05`)
- E-100 C1 gateway crash after WAL write before WAL fsync (`BeforeWALSync`)
- E-101 C2 gateway crash after WAL fsync before reply (`BeforeClientAck`)
- E-102 C3 storage node crash after temp written (`AfterShardTempWritten`)
- E-103 C4 storage node crash after fsync before rename (`AfterShardFsync`)
- E-104 C5 power loss after rename before directory sync (`AfterShardRename`)
- E-105 C6 gateway crash after all shard acks before Commit (`AfterAllShardsAcked`)
- E-106 C7 gateway crash after Raft commit before WAL reset (`AfterRaftCommit`)
- E-107 Storage node restart (temps on disk), E-108 gateway restart with a dirty WAL,
  E-109 torn WAL tail, E-110 disk full on one node, E-111 raft.db unopenable on one node
- E-112 reader observes 404/ErrCorrupt from a live node and reports the slot (`POST /v1/bad-slot`)
- E-113 dedup hit on an entry with fewer than k live slots
- E-114 truncate down then write past the truncation point before a flush
- E-115 shard DELETE arrives with a stale fence (receiver's applied index < header, or file younger than grace, or slot still placed on self)
- E-116 leader S3 sweep lists the bucket
- E-117 Sweep attempted while the node is not `Healthy` / behind the leader's commit index
- E-118 flush completes while new extents arrived (WAL compaction)
- E-119 policy migration PUTs the new encoding under keys `hex.k'-m'.idx` while old `hex.k-m.idx` files exist
- E-120 a gateway writes or truncates through an overlay entry that predates another gateway's Commit (its recorded base size is stale)

**5. Transition table.**

Columns: `Cell`, `From` state, `Event`, `To` state, observable `Outcome`, `Tests` that prove
the cell. "FILE_SYNC" means the NFS WRITE reply; "EIO/EROFS/ESTALE" are NFS3 statuses the
client sees; S3 equivalents are 200/500/503. Shard-level cells use shard states.

## Transition table

| Cell | From | Event | To | Outcome | Tests |
|---|---|---|---|---|---|
| L-1 | (none) | E-1 WRITE at 0, new file | Dirty | WAL record fsynced, FILE_SYNC; size = len | `TestVFS_PerRPCOpenCloseSequence`, `TestVFS_OracleRandomOps` |
| L-2 | Committed | E-2 WRITE inside committed chunk | Dirty (partial extent over Committed base) | FILE_SYNC; read-your-writes shows merged bytes; FSM unchanged until flush | `TestVFS_WriteAtNonZeroOffset_PreservesPrefix`, `TestVFS_OracleRandomOps` |
| L-3 | Committed | E-3 WRITE at EOF | Dirty (tail chunk) | FILE_SYNC; overlay size grows | `TestVFS_OracleRandomOps` |
| L-4 | Committed | E-4 WRITE past EOF | Dirty (target chunk); intervening indices become HashZero at flush | FILE_SYNC; gap reads zeros before and after flush | `TestVFS_WritePastEOF_CreatesHole_ReadsZeros` |
| L-5 | Committed | E-5 WRITE spanning boundary | Dirty ×2 | FILE_SYNC; both chunks flushed; bytes correct across the boundary | `TestVFS_WriteSpanningChunkBoundary` |
| L-6 | Dirty | E-6 overlapping WRITE | Dirty | FILE_SYNC; last writer wins within overlap | `TestOverlay_OverlappingWrites_LastWins`, `TestVFS_OracleRandomOps` |
| L-7 | (none) | E-7 aligned full-chunk WRITE | Dirty (full cover) | FILE_SYNC; flush assembles without reading base (no RMW fetch) | `TestVFS_AlignedFullChunkWrite_NoRMW` |
| L-8 | Committed | E-8 truncate down mid-chunk | Dirty (tail rewrite pending, `sizeSet`, `minSize`) | SETATTR OK; flush includes the straddling chunk in `Writes` and sets `HasTruncate` | `TestVFS_TruncateDown_DropsChunkRefs`, `TestFlusher_TruncateIncludesStraddlingChunk`, `TestFSM_Commit_ErrNeedTailRewrite` |
| L-9 | Committed | E-9 truncate down to boundary | Committed (dropped indices Refs−1 → Tombstoned if 0) | SETATTR OK; `Chunks` shortened at Commit, no data upload | `TestVFS_TruncateDown_Aligned_NoData` |
| L-10 | Committed | E-10 truncate to 0 | Tombstoned (all chunks Refs−1) if unshared | SETATTR OK; `Chunks == nil`, Size 0 | `TestVFS_TruncateToZero` |
| L-11 | Committed | E-11 truncate up | Committed (short tail) + HashZero indices | SETATTR OK; reads zero-fill beyond stored Size | `TestVFS_TruncateUp_Zeros`, `TestReader_ZeroFillShortChunk` |
| L-12 | Dirty | E-12 truncate below dirty extents | Dirty (extents trimmed) | WAL truncate record; trimmed bytes never flushed | `TestOverlay_TruncateTrimsExtents` |
| L-13 | Dirty | E-13 unlink before flush | (dropped) | WAL deleted; nothing committed; no chunk entry created | `TestVFS_UnlinkDirtyFile_DropsWAL` |
| L-14 | Committed | E-13 unlink (shared chunk) | Committed (Refs−1 ≥ 1) | REMOVE OK; chunk kept for the other file | `TestFSM_RefCount_SharedChunkAcrossFiles` |
| L-15 | Committed | E-13 unlink (sole reference) | Tombstoned | REMOVE OK; `TombstonedAt = Now`; shards remain | `TestFSM_Tombstone_OnRefsZero` |
| L-16 | Committed | E-14 rename | Committed | RENAME OK; ino, handle, chunks unchanged | `TestFSM_Rename_CrossDirKeepsIno`, `TestNFS_RenameKeepsHandleValid` |
| L-17 | Committed (dest file's chunks) | E-15 rename over existing | Committed/Tombstoned per Refs | RENAME OK; dest handle ESTALE; dest chunk refs dropped | `TestFSM_Rename_OverFile_DropsDestRefs` |
| L-18 | Dirty | E-14 rename of dirty file | Dirty | RENAME OK; later flush commits to the same ino | `TestVFS_RenameDirtyFile_FlushCommitsSameIno` |
| L-19 | Committed | E-16 identical content again | Committed (Refs+1) | second file costs no upload; stored bytes unchanged | `TestVFS_DedupIdenticalChunks` |
| L-20 | Tombstoned | E-16 identical content before GC | Revived → Committed | no upload; `TombstonedAt = 0`; removed from Tombs | `TestVFS_DedupRevivesTombstone_NoUpload`, `TestFSM_Revive` |
| L-21 | Committed | E-17 two gateways, disjoint chunks | Committed (both) | both Commits land; the second rebases on ErrConflict with `NewSize = max(newBase.Size, maxExtentEnd)`, so a concurrent append is preserved; file correct | `TestVFS_ConcurrentGateways_DisjointChunksBothLand`, `TestFlusher_ConflictRebase_PreservesConcurrentAppend` |
| L-22 | Committed | E-18 two gateways, same chunk | Committed (second committer's version); the first committer's chunk is Tombstoned when its last ref is replaced | exactly one whole version visible; size correct; no torn mix; a rebased truncate wins over a concurrent append | `TestVFS_ConcurrentGateways_SameChunkOneWins`, `TestFlusher_ConflictRebase_TruncateWins` |
| L-23 | Dirty | E-19 MaxDirty reached | Dirty | every WRITE returns NFS3ERR_IO (`EIO`) while total dirty bytes ≥ `MaxDirty`; existing dirty data intact; drains through the flush triggers | `TestVFS_MaxDirty_ReturnsEIO` |
| L-24 | (none) | E-20 S3 PUT | Dirty → … → Committed | 200 only after `Close` (WAL fsync); readable via NFS identically | `TestS3_PutThenNFSRead_Identical`, `TestS3_PutAckAfterClose` |
| L-25 | Committed (parts) | E-20 multipart Complete | Committed (spliced chunk list) | no data copy; ETag `md5(md5s)-N`; parts dir removed | `TestS3_MultipartComplete_SplicesChunks` |
| L-26 | Dirty | E-21 idle 2 s | Assembled | flush begins; WRITEs continue into a fresh extent set | `TestFlusher_IdleTrigger` |
| L-27 | Dirty | E-22 inode ≥ 32 MiB | Assembled (full chunks only) | only entirely dirty full-size chunks are committed; every dirty byte outside a committed chunk (the partial tail, a partially dirty head chunk, any partially dirty chunk between extents) stays Dirty as the residual; `NewSize = max(base.Size, end of last fully flushed chunk)`, never ahead of committed data | `TestFlusher_FileDirtyHiTrigger`, `TestFlusher_PartialFlush_SizeNeverAheadOfData`, `TestFlusher_PartialFlush_UnalignedAppend_KeepsHeadChunk`, `TestFlusher_PartialFlush_TwoExtents_KeepsPartialChunks` |
| L-28 | Dirty | E-23 total ≥ 256 MiB | Assembled (largest inodes first) | total dirty falls below threshold; smaller inodes wait for their own trigger | `TestFlusher_TotalDirtyHiTrigger` |
| L-29 | Dirty | E-24 shutdown | Committed before exit | `FlushAll` blocks shutdown until committed or deadline; residual stays in WAL | `TestFlusher_ShutdownFlushesAll` |
| L-30 | Dirty (partial) | E-25 assemble needs base | Assembled | base chunk fetched via `Reader.ReadChunk` and overlaid | `TestFlusher_AssembleRMW` |
| L-31 | Dirty (partial) | E-25 base unreadable (< k) | Dirty | flush error, backoff 1–60 s; WRITEs still FILE_SYNC | `TestFlusher_AssembleFails_BaseUnreadable_StaysDirty` |
| L-32 | Assembled | E-26 all zero | (HashZero, no entry) | `Writes[i].Hash = HashZero`; no shards; no chunk entry | `TestFlusher_AllZeroChunk_BecomesHashZero` |
| L-33 | Assembled | E-27 hash exists live | DedupHit | no upload; Commit Refs+1 | `TestVFS_DedupIdenticalChunks` |
| L-34 | Assembled | E-28 hash exists tombstoned | DedupHit → Revived | no upload; `TombstonedAt = 0` at Commit | `TestVFS_DedupRevivesTombstone_NoUpload` |
| L-35 | Assembled | E-29 ≤ 64 KiB | Uploading (k=1, m) | m+1 full replicas placed | `TestFlusher_SmallChunk_UsesReplication` |
| L-36 | Assembled | E-30 > 64 KiB | Uploading (K, M) | K+M shards of `ShardLen` each | `TestFlusher_LargeChunk_UsesPolicyKM` |
| L-37 | Assembled | E-31 policy changed | Uploading (new K, M) | new chunks use `Vol.(K,M)`; old chunks untouched here | `TestFlusher_UsesCurrentPolicyVersion` |
| L-38 | Assembled | E-32 tail rewrite needed | Uploading (tail included) | tail chunk hashed and included in `Writes` | `TestFlusher_TailRewriteIncluded` |
| L-39 | Uploading | E-33 all k+m acks | Placed(k+m) | ready to Commit with full placement | `TestCluster_WriteReadAllNodes` |
| L-40 | Uploading | E-34 one PUT fails, re-place succeeds | Placed(k+m) | second `Place` excluding failed node; k+m distinct | `TestFlusher_PutRetryReplaces` |
| L-41 | Uploading | E-34 PUT fails after retry, acks ≥ k | Placed(n<k+m) | slot recorded 0; Commit yields Degraded; `status` under-replicated +1 | `TestCluster_NodeDiesMidFlush_RepairRestoresRedundancy`, `TestFlusher_LiveEqualsK_CommitsDegraded` |
| L-42 | Uploading | E-34 acks < k | Dirty | flush error; landed shards are Orphan (young); retry with backoff | `TestFlusher_FewerThanKAcks_StaysDirty` |
| L-43 | Uploading | E-35 PUT timeout | as E-34 | timeout counted as failure; slot re-placed or 0 | `TestFlusher_PutTimeout_TreatedAsFailure` |
| L-44 | Uploading | E-36 507 ENOSPC | as E-34 (node excluded this flush) | `ErrFull`; node marked full for placement until free bytes recover | `TestRPC_PutENOSPC_507`, `TestFlusher_ENOSPC_ReplacesSlot` |
| L-45 | Uploading | E-37 400 hash mismatch | Uploading (retry once same node) | receiver stored nothing; second attempt or slot 0 | `TestRPC_HashMismatchRejected`, `TestFlusher_HashMismatch_RetryOnce` |
| L-46 | Uploading | E-38 403 | as E-34 | slot fails; `rpc_auth_denied` metric; `check` warns about membership mismatch | `TestFlusher_Auth403_FailsSlot` |
| L-47 | Uploading | E-39 409 cluster-id | as E-34 | slot fails; `check` reports foreign cluster | `TestRPC_ClusterIDMismatch409` |
| L-48 | Uploading | E-40 slow S3 past deadline, acks ≥ k | Placed(n<k+m) | WAL drain not stalled; S3 slot 0, filled by repair | `TestS3Node_SlowBucket_DoesNotStallFlush` |
| L-49 | Uploading | E-41 duplicate PUT (racing gateways) | Placed | 200; identical payload ⇒ mtime refreshed (grace extended), different payload ⇒ file replaced atomically | `TestStore_ConcurrentPutSameHash`, `TestStore_DupPutRefreshesMtime`, `TestStore_DupPutDifferentPayload_Overwrites` |
| L-50 | Uploading | E-42 self in placement | Placed | local `Store.Put`, no RPC; chunk cached | `TestFlusher_SelfSlotUsesLocalStore` |
| L-51 | Placed(k+m) | E-45 Commit OK | Committed | `WaitApplied`; extents cleared; other gateways see new version | `TestCluster_WriteReadAllNodes`, `TestVFS_CloseFlushIsDurable` |
| L-52 | Placed(n<k+m) | E-45 Commit OK | Degraded | committed with slot 0; `status` degraded; the 0 slot is filled on the next scan pass as soon as a live spare node exists (no hold-down) | `TestFlusher_DegradedCommitRecordsZeroSlots`, `TestRepair_ZeroSlotFilledWithoutHoldDown` |
| L-53 | Placed | E-46 ErrConflict | Placed → Committed | reload base, rebase per chunk (dedup makes untouched chunks free), re-Commit | `TestFlusher_ConflictRebase` |
| L-54 | DedupHit | E-47 ErrUnknownChunk | Uploading → Committed | hash was Collected between dedup check and Commit; re-upload then Commit | `TestFlusher_UnknownChunk_ReUpload` |
| L-55 | Placed | E-48 ErrNotExist | (dropped); shards Orphan | file quarantined meanwhile; WAL dropped; Orphans swept after grace | `TestFlusher_CommitErrNotExist_DropsWAL` |
| L-56 | Placed | E-49 ErrNeedTailRewrite | Assembled (tail) → Placed | flusher includes tail chunk and retries | `TestFlusher_TailRewriteRetry`, `TestFSM_Commit_ErrNeedTailRewrite` |
| L-57 | Placed | E-50 ErrNotLeader | Placed → Committed | forwarded to new leader; applied exactly once | `TestCluster_ForwardRetriesOnlyOnNotLeader` |
| L-58 | Placed | E-51 ErrLeadershipLost/timeout | Placed (outcome unknown) → Committed | the gateway (not the forwarding layer) retries the same Commit: if it had applied, ErrConflict + reload shows own hash → done; else applies; every retry re-runs the PUTs first (the chunk is registered only by the Commit that applies), refreshing the shards' mtime so the sweep cannot collect them | `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`, `TestFlusher_LateRetry_RePutsBeforeCommit` |
| L-59 | Placed | E-52 no quorum | Dirty (retained) | flush error; metadata ops EROFS; WRITEs still buffered | `TestCluster_NoQuorum_WritesEROFS` |
| L-60 | Committed | E-53 snapshot restore on follower | Committed | restored entry identical; FSM hash equal | `TestCluster_RejoinAfterLongAbsence_SnapshotInstall`, `TestFSM_ApplySnapshotRestoreEquivalence` |
| L-61 | Committed | E-54 leader change | Committed | no chunk-level effect; repair queue rebuilt by new leader from FSM | `TestCluster_LeaderChange_NoChunkEffect` |
| L-62 | Committed | E-55 WaitApplied | Committed | flushed extents cleared; `wal.Reset()` when inode clean | `TestFlusher_CommitFlushed_ResetsWAL` |
| L-63 | Committed | E-56 read, all reachable | Committed | first k by `Rank` (self first, then RTT; data slots before parity at equal rank); decode only if a data shard is missing | `TestReader_FetchFirstK` |
| L-64 | Committed | E-57 read, m unreachable | Committed | decode from any k; latency higher; no error | `TestCluster_KillMNodes_AllDataReadable` |
| L-65 | Committed | E-58 read, m+1 unreachable | Committed | EIO; no partial/garbage data; other chunks unaffected | `TestCluster_KillMPlusOne_ReadsFailCleanly` |
| L-66 | Committed | E-59 cache hit | Committed | no network; bytes verified against Hash on insert | `TestReader_CacheHit` |
| L-67 | Committed | E-60 cache corrupt | Committed | entry evicted; refetched from shards | `TestCache_CorruptEntryEvicted` |
| L-68 | Committed | E-61 fetched shard ErrCorrupt/404 | Degraded | next slot pulled; decode succeeds; reader sends `POST /v1/bad-slot {key}` to the leader; repairer treats the slot as bad after a `HEAD` re-check | `TestReader_CorruptShard_FallsBack`, `TestReader_ReportsBadSlot`, `TestCluster_TornShardOnOneNode_ReadDecodes_AndRepairs` |
| L-69 | Committed | E-62 decoded hash mismatch | Degraded | EIO + log; `Verify` on all k+m finds the bad shard; repair | `TestReader_DecodedHashMismatch_EIO` |
| L-70 | Committed | E-63 read beyond stored Size | Committed | zero-filled bytes (truncate-up semantics) | `TestReader_ZeroFillShortChunk` |
| L-71 | (HashZero) | E-64 read | (HashZero) | zeros; no fetch; no cache | `TestReader_HashZeroNoFetch` |
| L-72 | Dirty | E-65 read on issuing gateway | Dirty | overlay bytes merged over base (read-your-writes) | `TestOverlay_ReadYourWrites` |
| L-73 | Committed (new) | E-66 read on other gateway before apply | Committed | previous whole version returned; never a mix | `TestCluster_StaleReadPreviousVersion` |
| L-74 | Degraded | E-56 read, ≥ k reachable | Degraded | bytes correct | `TestLifecycle_Degraded_ReadOK` |
| L-75 | Committed | E-67 Refs → 0 | Tombstoned | in `Tombs`; `TombstonedAt = Now` | `TestFSM_Tombstone_OnRefsZero` |
| L-76 | Tombstoned | E-68 GCDelete before grace (on the FSM clock) | Tombstoned | not a victim; shards untouched; a lagging leader clock does not stall it | `TestGC_CollectsAfterGrace_RespectsFence_AndSweep`, `TestGC_LeaderClockBehind_DoesNotStallGC`, `TestFSM_GCDelete_Boundary` |
| L-77 | Tombstoned | E-69 GCDelete after grace (on the FSM clock) | Collected | victims returned; DELETE sent to every non-zero slot; `gc_bytes_reclaimed` | `TestGC_CollectsAfterGrace_RespectsFence_AndSweep`, `TestGC_LeaderClockBehind_DoesNotStallGC` |
| L-78 | Tombstoned | E-28 referenced by Commit | Revived → Committed | `Refs = 1`, `TombstonedAt = 0`; no upload | `TestFSM_Revive`, `TestVFS_DedupRevivesTombstone_NoUpload` |
| L-79 | Collected | E-70 DELETE RPC fails | Orphan (that node) | sweep removes it after grace; `check` counts orphan shards | `TestGC_DeleteRPCFails_SweepCatches` |
| L-80 | Orphan (young) | E-71 Sweep | Orphan | kept (I-14); may be committed shortly | `TestCluster_GCDoesNotDeleteInflightUpload`, `TestSweep_KeepsYoungOrphan` |
| L-81 | Orphan (old) | E-72 Sweep | Swept | deleted; `sweep_deleted` metric | `TestSweep_DeletesOldOrphan` |
| L-82 | Committed (orphan shard on old node) | E-73 Sweep, placement names another node | Committed; orphan shard Swept | chunk unaffected; old node frees space | `TestSweep_OldNotPlacedHereDeleted` |
| L-83 | Committed | E-74 Sweep, placed on self | Committed | kept regardless of age | `TestSweep_KeepsPlacedShard` |
| L-84 | Tombstoned | E-75 Sweep | Tombstoned | kept (entry exists); only GCDelete may remove | `TestSweep_KeepsTombstonedShard` |
| L-85 | Uploading | E-71 Sweep runs concurrently | Uploading | young shards kept; Commit succeeds | `TestCluster_GCDoesNotDeleteInflightUpload` |
| L-86 | Committed | E-76 node dead < fill_after | Committed | reads skip the slot; `status` shows the node unreachable with its missing slots and the chunk as degraded (under-replicated now) but not repair-pending; no repair | `TestRepair_DeadNodeHoldDown_ThenReplaced`, `TestReport_ReflectsClusterState` |
| L-87 | Committed | E-77 node dead > fill_after | Degraded | repair queued; `check` lists affected chunks/files | `TestRepair_FillAfterTriggers` |
| L-88 | Degraded (dead node) | E-79 node returns before repair | Committed | no work; slot reachable again | `TestRepair_NodeReturns_CancelsRepair` |
| L-89 | Degraded | repair run (read k, encode, PUT, SetPlacement) | Repairing → Committed | k+m slots on distinct live nodes; under-replicated −1 | `TestCluster_NodeDiesMidFlush_RepairRestoresRedundancy` |
| L-90 | Repairing | E-89 repair PUT fails | Degraded | stays queued; next scan retries; rate limited | `TestRepair_PutFails_Retries` |
| L-91 | Repairing | E-90 node returns with old shard mid-repair | Committed (new placement) | `SetPlacement` wins; old copy is Orphan → Swept | `TestRepair_RaceWithReturningNode` |
| L-92 | Degraded | E-78 node dead > evict_after | Degraded | `MemberLost` event; `status` marks the node lost and prompts `node remove --force` | `TestProber_LivenessAndHoldDowns`, `TestStatus_SuggestsRemoveAfterEvict` |
| L-93 | Committed | E-80 node drain | Degraded (migration) | node excluded from new placement; repairer relocates every slot; until a replacement is placed the slot stays on the draining node, whose live shard counts as readable (I-65) | `TestRepair_DrainMovesAllSlotsBeforeRemove`, `TestRepair_DrainNoReplacement_KeepsShard`, `TestRepair_DrainingHolderIsReadable` |
| L-94 | Degraded (drain) | relocation done | Committed; old shards Relocated → Deleted | `node remove` now allowed | `TestRepair_DrainMovesAllSlotsBeforeRemove` |
| L-95 | Committed (any slot on node) | E-81 node remove | Committed | refused by the FSM (`ErrNodeNotEmpty`) with the count of slots still on the node; `drain` first | `TestCluster_JoinDrainRemove`, `TestFSM_NodeRemove_RefusedWhileHoldingSlots` |
| L-96 | Committed | E-82 node remove --force | Degraded | every slot on it is set to 0 by `SetPlacement` before the node is removed; repair fills from the other k | `TestCluster_JoinDrainRemove` |
| L-97 | Committed | E-83 join | Committed | no data moves (placements and shard inventories unchanged); new node is a placement candidate | `TestCluster_JoinDoesNotMoveData` |
| L-98 | Committed | E-84 set-capacity | Committed | no data moves; weight changes future placements; ErrFull threshold changes | `TestCluster_SetCapacityDoesNotMoveData` |
| L-99 | Committed | E-85 IP change | Committed | placement by `NodeIdx` unaffected; the node re-registers its address (`update_addr`) so every peer dials the new one, then its `serve` exits with `ErrAddressChanged` for the service manager to restart it with listeners bound to the new address | `TestCluster_UpdateAddr_ChangesRPCAddr` |
| L-100 | Committed | E-86 set-redundancy | Degraded (policy) | chunk (K,M) ≠ `meta.TargetPolicy` (small chunks follow M only); migration progress in `volume describe` | `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK`, `TestRepair_SmallChunk_SetRedundancy_Migrates` |
| L-101 | Degraded (policy) | migration step | Repairing → Committed | read k old, encode new, PUT new slots, SetPlacement, DELETE old after commit; never < k available | `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK` |
| L-102 | Repairing (policy) | leader crash mid-migration | Degraded (policy) | old placement still committed; new leader redoes; orphans swept | `TestRepair_MigrationCrash_Resumes` |
| L-103 | Degraded | E-87 live < k+m | Degraded (fewer 0 slots) | repair fills what it can (partial fill), keeps scanning; `status` warns insufficient nodes | `TestRepair_PartialFillThenComplete` |
| L-104 | Committed (S3 slot) | E-88 S3 slow/unreachable | Committed / Degraded after fill_after | reads rank devices first; S3 used only when needed | `TestS3Node_DeviceLost_ReadsFromBucket` |
| L-105 | Committed | E-91 1..m shards corrupt | Degraded | read decodes; Quarantined; repair rewrites | `TestCluster_TornShardOnOneNode_ReadDecodes_AndRepairs` |
| L-106 | Committed | E-92 m+1 corrupt | Degraded | EIO (never garbage); the scrub counts the chunk unrepairable and `check` is not OK and lists the files | `TestCheck_MPlusOneCorrupt_NotOK`, `TestReader_DecodedHashMismatch_EIO` |
| L-107 | Committed | E-93 header mismatch | Degraded | `ErrCorrupt`; file quarantined; slot repaired | `TestStore_Get_CorruptedBitFlip`, `TestStore_Get_ZeroFilled` |
| L-108 | Committed | E-94 truncated shard | Degraded | `ErrCorrupt` (not short read) | `TestStore_Get_Truncated`, `TestStore_TornWrite_DetectedOnRead` |
| L-109 | Committed | E-95 scrub OK | Committed | shards Verified; `scrub_ok` metric | `TestScrub_VerifyOK` |
| L-110 | Committed | E-96 scrub detects corruption | Degraded → Committed | Quarantined → Repaired; `scrub_repaired` | `TestScrub_RepairsCorrupt` |
| L-111 | Committed | E-97 shard file missing | Degraded | GET 404 → treated as slot missing → repair | `TestRepair_MissingShardFile` |
| L-112 | Committed (S3 slot) | E-98 object missing | Degraded | HEAD/GET 404 → repair re-PUTs | `TestS3Node_ObjectMissing_Repairs` |
| L-113 | Dirty | E-100 C1 crash before WAL fsync | (none) or Dirty | write not acked; after restart content is exactly pre-write; torn record ignored | `TestCrash_AtEveryPoint`, `TestWAL_ReplayStopsAtTornTail` |
| L-114 | Dirty | E-101 C2 crash after WAL fsync | Dirty (replayed) | acked; replay on restart; flushed; readable everywhere | `TestCrash_AtEveryPoint`, `TestVFS_RestartWithDirtyWAL_Replays` |
| L-115 | Uploading | E-102 C3 node crash, temp written | Uploading (slot fails) | temp never visible; removed on `Store.Open` | `TestStore_CrashBeforeRename_LeavesNoVisibleShard` |
| L-116 | Uploading | E-103 C4 node crash after fsync before rename | Uploading (slot fails) | same as C3 | `TestStore_CrashBeforeRename_LeavesNoVisibleShard` |
| L-117 | Uploading | E-104 C5 power loss after rename before dir sync | Uploading (slot not acked) | no ack was sent, so the sender re-PUTs; the file either survived (dup PUT refreshes it) or is gone (re-created); never a half file, never an acked-but-lost shard | `TestStore_CrashAfterRenameBeforeDirSync_NotAcked`, `TestCrash_AtEveryPoint` |
| L-118 | Placed | E-105 C6 gateway crash after acks before Commit | Dirty (replay) ; landed shards Orphan | replay re-flushes; re-PUT returns 200 exists; Commit; no duplicate entries | `TestCrash_AtEveryPoint` |
| L-119 | Committed | E-106 C7 crash after commit before WAL reset | Committed | replay re-flushes same bytes → DedupHit → Commit (ErrConflict → reload shows identical) → WAL reset | `TestCrash_AtEveryPoint`, `TestFlusher_ReplayAfterCommit_Idempotent` |
| L-120 | any | E-107 storage node restart | unchanged | Durable shards intact; temps swept; `Used` recomputed | `TestStore_OpenSweepsTemps`, `TestStore_OpenSeedsUsed` |
| L-121 | Dirty | E-108 gateway restart | Dirty | WAL replayed in order; truncate records honoured | `TestVFS_RestartWithDirtyWAL_Replays` |
| L-122 | Dirty | E-109 torn WAL tail | Dirty (prefix) | replay stops at first bad crc; earlier records intact | `TestWAL_ReplayStopsAtTornTail` |
| L-123 | Uploading | E-110 disk full on one node | as E-36 | 507; no partial shard file left | `TestStore_DiskFull_NoPartialShard` |
| L-124 | Committed | E-111 raft.db unopenable on one node | Committed | that node fails to start with a clear error naming the file; others unaffected | `TestOpen_CorruptRaftDB_FailsClearly` |
| L-125 | (shard none) | PUT begins | Temp | `.tmp-*` in target dir; not listed by `Walk` | `TestStore_PutGetRoundTrip` |
| L-126 | Temp | fsync + rename | Durable | visible; `Has` true; `Used` += size | `TestStore_PutGetRoundTrip` |
| L-127 | Temp | process crash | (removed) | `Store.Open` deletes `.tmp-*` | `TestStore_OpenSweepsTemps` |
| L-128 | Durable | Get | Verified | header hash recomputed and matched on every read | `TestStore_Get_Verifies` |
| L-129 | Durable | bit flip / zero-fill / truncation | Corrupt → Quarantined | `ErrCorrupt`; file moved to `quarantine/<key>.<unixnano>` (cap 64, oldest removed), never served; `shards_corrupt` +1 | `TestStore_GetVerifiesHash_Corrupted` |
| L-130 | Quarantined | repair PUT | Repaired (Durable) | same key rewritten; slot reachable again | `TestScrub_RepairsCorrupt` |
| L-131 | Durable | node offline | Unreachable | `Live()` excludes the node and the reader never dials its slot (no dial or header timeout spent on it) | `TestReader_SkipsDeadHolders`, `TestProber_LivenessAndHoldDowns` |
| L-132 | Unreachable | node online | Reachable | probe success within 5 s cycle | `TestProber_Recovers` |
| L-133 | Durable | SetPlacement to other node | Relocated | old copy is Orphan; swept after grace or deleted by repairer | `TestSweep_StalePlacementShard` |
| L-134 | Durable | GCDelete DELETE with valid fence (applied index ≥ header, not placed on self, file older than grace) | Deleted | `Used` −= size; `Has` false | `TestGC_CollectsAfterGrace`, `TestRPC_DeleteFence_Accepts` |
| L-135 | Durable (orphan, old) | Sweep | Swept | deleted | `TestSweep_DeletesOldOrphan` |
| L-136 | Durable | duplicate PUT | Durable | identical payload: 200, mtime refreshed, no rewrite; different payload (e.g. stale encoding): 200, file replaced atomically | `TestStore_DupPutRefreshesMtime`, `TestStore_DupPutDifferentPayload_Overwrites` |
| L-137 | Durable (placed on self) | E-44 DELETE for a placed key | Durable | refused `412`; `check` reports mis-sent delete | `TestRPC_DeleteFence_RefusesPlacedShard412` |
| L-138 | Durable | PUT with capacity exceeded | Durable (new one rejected) | `ErrFull` → 507; existing shards untouched | `TestStore_CapacityEnforced` |
| L-139 | Durable (S3) | object deleted externally | (missing) | HEAD 404 → chunk Degraded → repair | `TestS3Node_ObjectMissing_Repairs` |
| L-140 | Durable | scrub sample | Verified | `scrub_ok`; event ring records verify | `TestScrub_VerifyOK` |
| L-166 | Committed | E-112 reader reports a bad slot | Degraded | leader `HEAD`s the slot; still bad ⇒ slot repaired on the next pass; `HEAD` ok ⇒ report dropped | `TestReader_ReportsBadSlot`, `TestRepair_ReportedBadSlotIsRepaired`, `TestRepair_BadSlotReportDroppedWhenHeadOK` |
| L-167 | Tombstoned/Degraded (< k present slots) | E-113 dedup hit | Uploading (fill) → Committed | flusher PUTs every slot that is not present (dead node, zero slot, or live node that answers `HEAD` 404) from the bytes it holds (re-place excluding holders), proposes `SetPlacement`, then Commit; never commits against an unreadable chunk | `TestFlusher_DedupDegradedEntry_FillsSlotsBeforeCommit`, `TestFlusher_DedupHit_LiveButMissingShards_RePuts` |
| L-168 | Committed | E-114 truncate down then write past the truncation point | Dirty (`minSize` < `size`) → Committed | flush emits `HashZero` for uncovered indices ≥ ceil(minSize/CS), rewrites the straddling chunk, sets `HasTruncate`/`TruncateTo`; old bytes never resurface; FSM drops refs beyond `TruncateTo` first | `TestVFS_TruncateThenGrow_OldBytesDoNotResurface`, `TestFSM_Commit_TruncateThenGrow_DropsOldChunks` |
| L-169 | Durable (device) | E-115 DELETE with stale fence | Durable | `412`; file kept; `Sweep` reconciles later | `TestRPC_DeleteFence_RefusesStaleIndex`, `TestRPC_DeleteFence_RefusesYoungShard` |
| L-170 | Orphan (S3 object) | E-116 leader S3 sweep | Swept | object older than grace and not placed on that node is deleted via the S3 client; younger objects kept | `TestS3Node_SweepUsesListPaging`, `TestS3Sweep_KeepsYoungObjects` |
| L-171 | Orphan | E-117 Sweep while not `Healthy` / behind leader index | Orphan | sweep skipped entirely; `sweep_skipped_unhealthy` +1 | `TestSweep_SkippedWhenNotHealthy` |
| L-172 | Dirty (fresh extents during flush) | E-118 flush completes | Dirty (compacted WAL) | WAL rewritten to the live extents and the current size, fsynced, renamed; acknowledged bytes and the acknowledged size unchanged; replay after crash yields the same overlay | `TestWAL_Compact_KeepsOnlyLive`, `TestOverlay_CommitFlushed_WithResidual`, `TestOverlay_TruncateUpDuringFlush_SurvivesReopen`, `TestOverlay_OracleRandomOps` |
| L-173 | Degraded (`(k,m)` ≠ policy) | E-119 migration PUTs under new keys | Repairing → Committed | new `hex.k'-m'.idx` files coexist with old `hex.k-m.idx`; `SetPlacement` switches; old keys deleted with the fence; at no instant < k decodable shards | `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK`, `TestStore_KeysWithDifferentEncodingCoexist` |
| L-178 | Committed | E-120 write/truncate via a gateway whose overlay entry is stale | Dirty → Committed | the overlay never pins a base size: a clean entry is dropped after its flush and a dirty one treats the committed size as current, so base bytes stay valid up to the committed size unless an explicit truncate cuts them, rewritten chunks keep their full length, `TruncateTo` is the requested point (never the stale base), and GETATTR reports the committed size while the entry has no truncate | `TestOverlay_StaleEntry_FollowsCommittedSize`, `TestCluster_StaleGatewayOverlay_KeepsOtherGatewaysBytes` |
| L-180 | Dirty (size raised by a truncate while a flush plan is outstanding) | E-118 flush completes | Dirty (compacted WAL, size kept) | the compacted WAL records the truncation point and the raised size, so a restart replays the acknowledged SETATTR size, not the smaller committed or truncated size | `TestOverlay_TruncateUpDuringFlush_SurvivesReopen`, `TestOverlay_OracleRandomOps` |
| L-181 | Dirty (flush plan outstanding) | E-12 truncate below the plan's extents | Dirty (plan bytes beyond the point dead) | SETATTR OK; reads never return plan bytes at or beyond the lowest truncation point since the snapshot; the commit still carries them and `minSize` hides them afterwards; an abort trims them | `TestOverlay_TruncateDuringFlush_HidesPlanBytes`, `TestOverlay_OracleRandomOps` |
| L-183 | Committed (holders live, shard files lost on some) | E-113 dedup hit | Uploading (fill) → Committed | the flusher checks presence with `HEAD` per live slot; with fewer than k present it PUTs the absent slots from its own bytes and proposes `SetPlacement` before the Commit; with k or more present it dedups and leaves the rest to scrub and repair | `TestFlusher_DedupHit_LiveButMissingShards_RePuts` |
| L-185 | Degraded (a missing slot, or a slot on a node dead inside `fill_after`) | repair pass with no live spare | Degraded | nothing is placeable, so nothing is proposed: no `SetPlacement`, no `partial` count, no event (I-65); the next pass retries | `TestRepair_DeadWithinFillAfter_NoRepeatProposal` |
| L-186 | Dirty | E-121 per-inode WAL ≥ 128 MiB | Assembled (full flush) | the flusher flushes the inode in full regardless of the idle timer so its WAL compacts (a rewritten region keeps dirty bytes small while the WAL grows); a WRITE that still arrives at the cap fails with `EIO`, never `EAGAIN`, and succeeds once the flush has drained the WAL | `TestFlusher_WALCapTriggersFlush`, `TestOverlay_WALCap_RejectsAtCap`, `TestOverlay_WALCap_Boundaries` |
| L-187 | Committed (metadata mutation on a gateway that lost its leader) | E-54 leader unreachable | Committed | the proposal is retried with backoff while the gateway had leader contact within the Stale threshold (30 s); once stale, or with no leader for that long, the mutation fails with `EROFS` (`NFS3ERR_ACCES`/`NFS3ERR_IO` on the wire, docs/07) and data WRITEs keep going into the WAL | `TestVFS_ProposeFails_RetriesUntilStale`, `TestCluster_IsolatedGateway_ReturnsEROFS_ReadsStillServed`, `TestNFS_ReadOnly_WireStatuses` |

**6. Forbidden transitions.**

Each row is a transition that must never be observed. Tests assert the negative (the model
checker, `TestCrash_AtEveryPoint`, and the FSM invariant checker fail if any appears in the
event ring or the final state).

## Forbidden transitions

| Cell | From | Event | To | Outcome | Tests |
|---|---|---|---|---|---|
| L-141 | Committed | GCDelete | Collected | forbidden: a chunk with Refs > 0 is never a victim | `TestFSM_GCDelete_NeverCollectsReferenced`, `TestModelCheck_Long` |
| L-142 | Temp | Get/Open by a reader | served | forbidden: temp files are never served or listed | `TestStore_TempNeverServed` |
| L-143 | Committed/Tombstoned | Unlink/Commit | Refs < 0 | forbidden: a decrement that would underflow is rejected with `ErrBadCommand`, never a panic, and leaves `State` unchanged | `TestFSM_RefcountUnderflow_IsBadCommand` |
| L-144 | Uploading/Repairing | Place/SetPlacement | two slots of one stripe on one node | forbidden (I-9) | `TestPlace_DistinctNodesPerStripe`, `TestFSM_SetPlacement_RejectsDuplicateNode` |
| L-145 | Orphan (young) | Sweep | Swept | forbidden (I-14): shards younger than grace are never swept | `TestSweep_KeepsYoungOrphan`, `TestCluster_GCDoesNotDeleteInflightUpload` |
| L-146 | Tombstoned (≥ k slots on live nodes) | Commit referencing hash | Uploading | forbidden: a Revived chunk with ≥ k live slots never requires re-upload (with < k live slots the flusher fills the missing slots first, L-167) | `TestVFS_DedupRevivesTombstone_NoUpload` |
| L-147 | Committed | membership change (join/remove/IP/capacity) | placement changed | forbidden: placement changes only via `SetPlacement` | `TestFSM_PlacementOnlyChangedBySetPlacement`, `TestPlace_StabilityOnMembershipChange` |
| L-148 | Placed | Commit proposed | visible on any gateway before Raft commit | forbidden (I-7) | `TestCluster_StaleReadPreviousVersion`, `TestCluster_SplitBrain_OnlyMajorityCommits` |
| L-149 | (none) | WRITE | FILE_SYNC before WAL fsync | forbidden (I-7): a WRITE whose record did not fsync is an error, holds no byte and replays nothing | `TestOverlay_WriteNotAckedWhenFsyncFails`, `TestCrash_AtEveryPoint` |
| L-150 | Committed | any | Refs > 0 with no inode referencing the hash | forbidden: Σ inode references == Refs | `TestFSM_InvariantChecker_RefsMatchReferences` |
| L-151 | Committed | any | len(Chunks) ≠ ceil(Size/CS) | forbidden (I-1) | `TestFSM_SetChunks_SizeAndChunkListConsistent` |
| L-152 | Assembled | all zero | shards stored for HashZero | forbidden (I-2) | `TestFlusher_AllZeroChunk_BecomesHashZero` |
| L-153 | (any inode) | Create after Unlink | same (ino, gen) reused | forbidden (I-3) | `TestFSM_InoGenNotReusedAfterUnlink` |
| L-154 | Corrupt | Get | bytes returned | forbidden: a shard whose payload hash mismatches is never returned | `TestStore_Get_CorruptedBitFlip` |
| L-155 | Uploading | acks < k | Commit proposed | forbidden: a Commit is never proposed with fewer than k durable shards | `TestFlusher_FewerThanKAcks_StaysDirty`, `TestCrash_AtEveryPoint` |
| L-156 | Assembled | Place | slot on a Draining/Removed node for a new chunk | forbidden | `TestPlace_ExcludesDrainingAndRemoved` |
| L-157 | Repairing (policy) | migration | old shards deleted before new placement committed | forbidden (I-15) | `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK` |
| L-158 | Durable (placed on self) | Sweep | Swept | forbidden (I-13): a shard named by committed placement is never swept | `TestSweep_KeepsPlacedShard`, `TestModelCheck_Long` |
| L-159 | any FSM state | Apply | result depends on wall clock, disk, or map iteration order | forbidden (I-4) | `TestFSM_DeterministicAcrossReplicas` |
| L-160 | Collected | Commit referencing hash | Committed without upload | forbidden: must raise `ErrUnknownChunk` | `TestFSM_Commit_UnknownChunkRejected` |
| L-161 | Committed (any slot on node) | node remove without --force | Degraded | forbidden: refused with `ErrNodeNotEmpty` | `TestCLI_NodeRemove_RefusesWhileHoldingSlots` |
| L-162 | Dirty | ErrConflict rebase | dirty extents lost | forbidden: rebase re-reads base but never drops dirty extents | `TestFlusher_ConflictRebase_KeepsAllExtents`, `TestVFS_OracleRandomOps` |
| L-163 | Committed (two versions) | stale read | bytes from two versions in one chunk | forbidden: a chunk is read whole from one committed hash | `TestCluster_StaleReadPreviousVersion` |
| L-164 | Tombstoned | Sweep | shards deleted before GCDelete | forbidden: only `GCDelete` after grace removes tombstoned shards | `TestSweep_KeepsTombstonedShard`, `TestGC_RespectsGrace` |
| L-165 | Degraded | repair | chunk bytes differ from original Hash after repair | forbidden: repair output is verified against Hash before SetPlacement | `TestRepair_VerifiesBeforeSetPlacement` |
| L-174 | Durable (device) | DELETE accepted with applied index < fence, or file younger than grace, or slot placed on self | Deleted | forbidden: the DELETE fence rejects it with `412` | `TestRPC_DeleteFence_RefusesStaleIndex`, `TestRPC_DeleteFence_RefusesYoungShard`, `TestRPC_DeleteFence_RefusesPlacedShard412` |
| L-175 | Committed | Commit without `HasTruncate` and `NewSize` < `Size` | Committed (shrunk) | forbidden: `ErrBadCommand`; a file shrinks only through an explicit truncate | `TestFSM_Commit_RejectsImplicitShrink` |
| L-176 | Degraded/Tombstoned (< k present slots) | dedup hit | Committed (no upload) | forbidden: an acknowledged write is never committed against a chunk with fewer than k present slots; presence means a live holder that still has the shard, not liveness alone | `TestFlusher_DedupDegradedEntry_FillsSlotsBeforeCommit`, `TestFlusher_DedupHit_LiveButMissingShards_RePuts` |
| L-177 | Orphan | Sweep while not `Healthy` | Swept | forbidden: a node whose chunk table may be behind never deletes shards | `TestSweep_SkippedWhenNotHealthy` |
| L-179 | Dirty (chunk partly dirty, below the last committed chunk) | E-22 partial flush | dirty bytes dropped | forbidden (I-7): a partial flush commits only entirely dirty chunks and keeps every other dirty byte as residual; acknowledged bytes are never neither committed nor dirty | `TestFlusher_PartialFlush_UnalignedAppend_KeepsHeadChunk`, `TestFlusher_PartialFlush_TwoExtents_KeepsPartialChunks` |
| L-182 | Committed | Commit with `NewSize` > `MaxFileSize(ChunkSize)` | Committed (unbounded chunk list) | forbidden (I-64): `ErrBadCommand` before anything is sized by `NewSize`; the gateway refuses the WRITE or SETATTR with `EFBIG` first, so no replica ever allocates per a hostile size | `TestFSM_Commit_NewSizeBounds`, `TestVFS_SizeBeyondMax_EFBIG` |
| L-184 | Committed (slot on a live draining node, no replacement available) | repair pass that improves another slot | slot zeroed and the draining node's shard deleted | forbidden (I-65): the draining node keeps its slot until a replacement is placed; a repair commit never lowers the readable slot count | `TestRepair_DrainNoReplacement_KeepsShard` |

## 7. How the matrix is tested

**Table-driven lifecycle suite** — `internal/testharness/lifecycle_test.go` holds one case
per reachable cell, run against the in-process `Cluster` (k=2, m=1, 4 nodes unless a case
overrides):

```go
type chunkRef struct{ ino meta.Ino; idx uint32; hash meta.Hash }
type lifecycleCase struct {
    cell   string                                   // "L-41"
    setup  func(t *testing.T, c *Cluster) chunkRef   // drive the chunk into the From state
    event  func(t *testing.T, c *Cluster, r chunkRef) // apply exactly one event (hooks, FaultNet, clock, CLI)
    expect func(t *testing.T, c *Cluster, r chunkRef) // assert To state via c.ChunkState(r), outcome via client/metrics
}
func TestLifecycle(t *testing.T) { for _, lc := range lifecycleCases { t.Run(lc.cell, ...) } }
```
`Cluster.ChunkState(r)` recomputes the state purely from the FSM copy, the shard stores and
liveness (the derivation rules of §2–§3); the observational states Repairing, Revived and
Quarantined are not derivable from those sources and are asserted through the event ring
instead. `tailraid chunk describe` prints the same derivation, and prints
`(event ring disabled)` for the history section when `dev.enabled` is off. The
table's `Tests` column must name `TestLifecycle` cells by their `// Lifecycle: L-n` comment
or the dedicated test named in the row; `scripts/check-invariants.sh` treats `L-n` like
`I-n` (defined in the two tables above, cited from `_test.go`).

**Chunk event ring** — in dev mode (`dev.enabled`), every node appends
`meta.ChunkEvent{At time.Time; Node NodeIdx; Kind EventKind; Detail string}` to a bounded ring
(last 64 events) per chunk hash, covering flush, PUT, Commit, read, repair, GC and sweep
events. Tests assert the *path* taken, not only the end state (e.g. L-54 must show
`DedupHit → ErrUnknownChunk → Upload → Commit`). `tailraid chunk describe <hash>` prints
state, (k,m), placement with per-slot reachability and verification, Refs, referencing files,
and the ring.

**Coverage recording** — each cell hit by any test, the model checker, or the soak suite is
recorded (`lifecycle_hits.json`); `make test-long` and the soak report print the cells never
exercised in that run, and the invariant script fails if a cell has no citing test at all.
The model checker additionally classifies every observed `(state, event, state')` triple
and fails on any triple not in §5 (forbidden by construction, I-57).

**Regression rule** — a bug fix adds a test with `// Regression: #<issue> L-n` naming the cell
whose outcome was wrong; if no cell fits, the table is extended first (new `L-n`), then the
test is written, then the fix.

## Invariants

- **I-56** — Every chunk referenced by an inode is in exactly one chunk state of §2, excluding the observational states Repairing, Revived and Quarantined, and that state is derivable from the FSM state plus the shard stores and liveness alone (no hidden gateway-only state except Dirty/Assembled/Uploading/Placed, which are confined to the issuing gateway and never affect what other gateways read). `meta.ChunkState` is that derivation and the only one in the code: `status`, `check`, `chunk describe` and the repair scanner all call it.
- **I-57** — Every transition observed in the chunk event ring matches a cell of the transition table; no forbidden transition is ever observed in any test, model-check or soak run.

## Tests

| Invariant | Tests |
|---|---|
| I-56 | `TestChunkState_Table`, `TestSmallChunkRule`, `TestCheck_MPlusOneCorrupt_NotOK`, `TestRepair_SmallChunk_SetRedundancy_Migrates`, `TestChunkDescribe_StateFilesAndEvents`, `TestReport_ReflectsClusterState` |
| I-57 | `TestModelCheck_Long`, `TestLifecycle_NoForbiddenTransitions`, `TestCrash_AtEveryPoint` |
