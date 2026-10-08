# 06 — Garbage collection and repair

Dedup means a chunk may be referenced by many files; erasure coding means a chunk's shards
live on several nodes that come and go. This document fixes how unreferenced data is
reclaimed without ever deleting referenced data, and how redundancy is restored after node
loss, drain, or a change of the volume's (k,m) policy.

## 1. Reference counting and tombstones

Inside the FSM (single-threaded, deterministic):

```go
type Chunk struct {
    Hash         Hash
    Size         uint32
    K, M         uint8
    Refs         uint32
    Placement    []NodeIdx   // len K+M; 0 = missing slot
    TombstonedAt int64       // FSM clock (max(cmd.Now, LastNow), see 02); 0 = live
}
State.Tombs map[Hash]struct{}  // index of chunks with Refs == 0
```

- `Commit` increments `Refs` for every hash newly referenced by the inode's chunk list and
  decrements for every hash dropped. `Refs == 0` → `TombstonedAt = now` (the FSM's
  monotonic clock, so a proposer with a slow clock cannot back-date a tombstone), add to
  `Tombs`. A decrement that would underflow is `ErrBadCommand`.
- A later `Commit` that references a tombstoned hash revives it (`TombstonedAt = 0`, remove
  from `Tombs`, `Refs = 1`). The flusher's dedup check treats tombstoned chunks as present
  **when at least k of their slots are on live nodes**; otherwise it fills the missing slots
  from the bytes it holds before committing (04 §4 step 6). Tombstoned chunks are never
  scanned by the repairer; revival is what repairs them.
- `Unlink`, `Rename`-over-existing and `Truncate` (via `Commit`) are the only paths that
  decrement.
- Invariant checked by `fsck` and by the FSM test suite: `Σ Refs == number of (inode,
  index) pairs referencing the hash` and `Tombs == {h : Refs == 0}`.

## 2. GC worker (leader only)

```go
type GC struct { st *meta.Store; prop meta.Proposer; shards chunk.ShardClient; grace time.Duration; clock clock.Clock }
func (g *GC) RunOnce(ctx context.Context) (deleted int, err error)
```

Every `gc_interval` (default 10 min) the leader proposes
`GCDelete{Before: fsmNow − grace}` with `grace = 10 min`, where `fsmNow` is the leader's
clock clamped by the FSM (02). The FSM deletes every chunk entry with `Refs == 0 &&
TombstonedAt < Before` and returns the victims `[]{Hash, K, M, Placement}`. The worker
then issues `DELETE /v1/shards/{key}` for each victim slot on **device** nodes with the
header `X-Tailraid-Log-Index: <index of the GCDelete>` (best effort; misses are caught by
`Sweep`). Because `Before` is in the command, every replica deletes the same entries at the
same log index (I-4).

**DELETE fence (device nodes).** The receiver deletes the file only if all of:
1. its own applied Raft index ≥ `X-Tailraid-Log-Index` (it has seen the GCDelete or
   SetPlacement that justifies the delete);
2. its chunk table has no entry for the hash, or the entry's `Placement[idx] != self` under
   that key's `(k,m)`;
3. the file's mtime is older than grace (`Store.DeleteIfOlder`).
Otherwise it answers `412 Precondition Failed` and keeps the file; the sender logs and moves
on (Sweep will reconcile). Rule 3 is what makes I-14 hold for GC as well as Sweep: a shard
that a flusher just re-PUT for a revived or re-uploaded chunk is younger than grace and
survives a stale DELETE, whatever the receiver's FSM lag.

**S3 nodes** are never sent DELETEs by the GC worker. They are cleaned only by the leader's
S3 sweep (§3) and a dup PUT to S3 always overwrites the object (fresh `LastModified`), which
is the S3 equivalent of the mtime refresh.

Why grace: a flusher may have PUT shards for a chunk hash and be about to `Commit`. If the
hash was tombstoned and GC removed the entry in between, the Commit fails with
`ErrUnknownChunk` and the flusher re-uploads (04 §4 step 10). Grace makes that race rare and
the DELETE fence makes it harmless; correctness does not depend on timing.

## 3. Sweep (every node, hourly)

```go
func Sweep(ctx context.Context, s *Store, self meta.NodeIdx, m *meta.Store, grace time.Duration) (deleted int, err error)
```

Precondition: the node's `Health == Healthy` **and** its applied index ≥ the leader's
commit index observed (via `/health`) at the start of the scan. A node that is on the
minority side, or has just returned and not yet installed the snapshot, skips the sweep;
its chunk table would call committed shards orphans.

For every shard file on this node's disk with `mtime < now − grace`:

- parse `key → (hash, k, m, idx)`;
- `c, ok := m.GetChunk(hash)`; delete the file if `!ok` **or** `(c.K, c.M) != (k, m)`
  **or** `c.Placement[idx] != self` (an **orphan shard**: uploaded but never committed, or
  left behind by a re-place or a policy migration).

A shard younger than grace is never touched (I-14): it may belong to an in-flight flush
whose Commit has not yet been applied locally. `Put` of an already-present identical shard
refreshes its mtime for the same reason. Sweep never consults the network; it uses only the
replicated chunk table, so a node that returns after a week cleans up exactly what the
cluster has re-placed elsewhere.

**S3 sweep (leader only, daily).** For each S3 node the leader lists the bucket prefix with
`ListObjectsV2` (paged) and deletes every object whose `LastModified` is older than grace
and whose key is not placed on that node in the chunk table. Only the leader runs it, and
only while `Healthy`. Because a dup PUT to S3 overwrites (fresh `LastModified`), an object a
flusher just re-uploaded is always younger than grace.

## 4. Repair scanner (leader only)

```go
type Repairer struct {
    st *meta.Store; prop meta.Proposer; peers meta.Peers; rd *vfs.Reader
    shards chunk.ShardClient; codecs *ec.Pool; rate *rate.Limiter; clock clock.Clock
}
func (r *Repairer) Scan(ctx context.Context) error   // one pass over ForEachChunk
```

A slot `i` of chunk `c` is **bad** when any of:

1. `c.Placement[i] == 0` (degraded write, or earlier failed re-place) — no hold-down: it
   is filled as soon as a live spare node exists;
2. the node is `Removed` or `Draining`;
3. the node has been dead (no successful probe) for longer than `fill_after` (10 min);
4. `(c.K, c.M) != (Vol.K, Vol.M)` and `c.K != 1` — the chunk predates a policy change
   (small chunks with `K == 1` follow `Vol.M` only: bad if `c.M != Vol.M`);
5. the slot was **reported bad** by a reader: any gateway whose read of slot `i` got `404`
   or `ErrCorrupt` from a live node sends `POST /v1/bad-slot {key}` to the leader
   (fire-and-forget, deduplicated per key for 10 min on the sender; the leader keeps a
   bounded set). Reported slots are re-verified by a `HEAD` before repair; a slot whose
   `HEAD` succeeds is dropped from the set. This is how a lost rename, a quarantined file
   or a stale shard gets repaired without waiting for the 1 % scrub sample.

A chunk with ≥ 1 bad slot and ≥ k good live slots is **repairable**. Repair of one chunk:

1. `chunk := rd.ReadChunk(hash)` (verifies the hash; needs k good shards).
2. Determine target `(k', m')`: current policy for case 4, else the chunk's own.
3. `newPlacement := Place(hash, liveCandidates − goodNodes(kept slots), needed)`. For case 4
   every slot is re-placed (under the new keys `hex.k'-m'.idx`, which cannot collide with
   the old files); for cases 1–3 and 5 only the bad slots, keeping good ones in place; slot
   `i` keeps index `i`.
4. `codec(k', m').Encode(chunk)`; PUT the new shards to their nodes. Slots whose PUT fails
   stay `0`: repair may fill **partially** (a chunk goes from 1 good slot to 3 of 5 rather
   than waiting for 5) and the next scan pass fills the rest. For case 4, however, the new
   placement must have ≥ k' acked slots before step 5, or the migration of that chunk is
   deferred to the next pass (the old encoding stays committed).
5. `Propose(SetPlacement{Hash, K: k', M: m', Placement: newPlacement})`.
6. Only after `WaitApplied`: `DELETE` old shards that are no longer in the placement, with
   `X-Tailraid-Log-Index` = the SetPlacement's index (DELETE fence, §2). Readers that hold
   a copied `Chunk` entry from before the migration may get `404` on an old slot; the read
   path refetches the entry once before giving up (04 §5).

Step ordering guarantees I-15's second clause: at every instant at least k shards decodable
under the *committed* placement exist, because the old placement stays committed until the
new shards are durable, and old shards are deleted only after the new placement is
committed.

Rate limiting: `--rate` (default 50 MB/s of shard traffic) via a token bucket; a scan pass
also yields between chunks so foreground IO is not starved. Progress is exposed as
`admin.RepairStatus{Pending, Repaired, Failed, BytesMoved, PolicyMigrated, PolicyTotal}`.

Chunks with < k good live slots are **unrepairable**; they are counted and listed by
`tailraid check --files` and retried on the next pass (a node may come back). Both repair
and the GC worker are leader-only and run only while the leader is `Healthy`.

## 5. Scrub

Every scan pass also verifies a sample of chunks (default 1 % per pass, so a full scrub
completes in ~100 passes; `tailraid check --scrub` forces 100 %): fetch **all** k+m shards,
`codec.Verify`. A `false` result or any `ErrCorrupt` from a node marks the offending slots
bad and repairs them via §4 (the chunk itself is decoded from a hash-verified subset).
A node that detects a header or hash mismatch at read time moves the file to
`quarantine/<key>.<unixnano>` (capped at 64 entries, oldest removed) and answers `404`
(I-11); the reader reports the slot (§4 case 5), so a corrupt shard is repaired on the next
scan pass rather than when the sample reaches it. Quarantined files exist for forensics only
and are never served.

## 6. Membership flows

**`node drain <n>`** → `NodeSetState{Draining}`. Draining nodes are excluded from
placement candidates immediately; the scanner treats their slots as bad (§4 case 2) and
moves them. `status` shows `draining: 1,234 slots remaining`.

**`node remove <n>`** → refused (`ErrNodeNotEmpty`) while the node still holds *any*
non-zero placement slot unless `--force` (which first proposes `SetPlacement` with slot 0
for those shards); then `NodeSetState{Removed}` is committed and `raft.RemoveServer` follows
(02). Removed nodes keep their `NodeIdx` forever (indexes are never reused) so historical
placements stay interpretable.

**`node add` / `join`** → `NodeUpsert{State: Active}`; the node is a candidate for new
chunks at once. No data moves toward it until the repairer has a reason to (no rebalancing
in barebones; capacity weighting in `Place` biases new writes).

## 7. Policy change: `volume set-redundancy --k K --m M`

1. Refused unless `K + M ≤ live Active nodes (devices or S3)` and `K ≥ 1, M ≥ 0, K + M ≤
   256`.
2. `SetVolume{K, M}` is committed; the FSM increments `PolicyVersion` itself when `K` or
   `M` changed. From this log index every new chunk uses the new policy (small chunks use
   `(1, M)`).
3. The scanner migrates existing chunks lazily (§4 case 4). Migration order: chunks with
   fewer effective parity first (a change from 2+1 to 3+2 migrates 2+1 chunks before any
   that already have 2 parity).
4. Progress: `PolicyMigrated / PolicyTotal` where `PolicyTotal` is the count of chunks whose
   `(K, M)` differed at the moment of the change; `volume describe` shows it and an ETA from
   the rate limiter.
5. A second policy change during migration simply retargets: the scanner compares every
   chunk with the current policy, never with the previous one.

Capacity: a change that increases `(K+M)/K` needs headroom; `set-redundancy` prints the
projected stored bytes and refuses if projected > Σ capacity unless `--force`.

## 8. fsck

`tailraid fsck [--fix] [--verify-data]` checks, from any node's FSM copy: refcount sums;
`len(Chunks) == ceil(Size/CS)` for every file; every parent chain reaches root and every
`Children` entry points to an inode whose `Parent`/`Name` agree; `Tombs ⇔ Refs == 0`;
placement distinctness; and, with `--verify-data`, decodes every chunk from its shards and
compares hashes (this is the "offline reconstruct" used by the soak suite's cross-view
check). `--fix` only re-derives refcounts and tombstones; it never deletes shards.

## Invariants

- **I-13** — No shard referenced by committed metadata is ever deleted: GC deletes only chunks with `Refs == 0` older than grace, repair deletes old shards only after the new placement is committed, and Sweep deletes only shards the replicated chunk table does not place on this node.
- **I-14** — A shard file (or S3 object) younger than the grace period is never deleted by Sweep, the S3 sweep, or a GC/repair DELETE, regardless of what the chunk table says.
- **I-15** — Repair restores k+m slots for any chunk with ≥ k good live shards, and migration to a new (k,m) never passes through a state in which fewer than k shards decodable under the committed placement exist.
- **I-41** — A tombstoned chunk re-referenced before GC is revived without re-upload, and its `Refs` is exact.
- **I-42** — Draining a node moves every one of its slots before `node remove` succeeds without `--force`; after removal no committed placement references the node except via `--force`-acknowledged degraded slots.
- **I-43** — `GCDelete` applied on any replica deletes exactly the same chunk entries (determinism via `Before` in the command).
- **I-44** — `fsck` reports zero findings on a cluster where every test in this document passed; any finding is a bug, not noise.

## Tests

| Invariant | Tests |
|---|---|
| I-13 | `TestCluster_GCDoesNotDeleteInflightUpload`, `TestRepair_DeletesOldShardsOnlyAfterCommit`, `TestSweep_OldPlacedHereKept`, `TestSweep_SkippedWhenNotHealthy`, `TestModelCheck_Long` |
| I-14 | `TestSweep_YoungShardKept`, `TestStore_DeleteIfOlder_KeepsYoung`, `TestRPC_DeleteFence_RefusesYoungShard`, `TestRPC_DeleteFence_RefusesStaleIndex`, `TestCluster_GCDoesNotDeleteInflightUpload` |
| I-15 | `TestCluster_NodeDiesMidFlush_RepairRestoresRedundancy`, `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK`, `TestCluster_TornShardOnOneNode_ReadDecodes_AndRepairs`, `TestRepair_ReportedBadSlotIsRepaired`, `TestRepair_PartialFillThenComplete` |
| I-41 | `TestFSM_RefCount_SharedChunkAcrossFiles`, `TestFSM_Commit_RevivesTombstonedChunk`, `TestVFS_DedupIdenticalChunks`, `TestFlusher_DedupHitWithFewerThanKLiveSlots_Fills` |
| I-42 | `TestRepair_DrainMovesAllSlotsBeforeRemove`, `TestCLI_NodeDrain_ThenRemove_MovesSlots` |
| I-43 | `TestFSM_DeterministicAcrossReplicas`, `TestFSM_GCDeleteDeterministic` |
| I-44 | `TestFsck_CleanClusterNoFindings`, `TestFsck_DetectsInjectedRefcountError` |
