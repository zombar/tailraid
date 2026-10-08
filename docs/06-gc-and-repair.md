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
    TombstonedAt int64       // unix nanos from the command's Now; 0 = live
}
State.Tombs map[Hash]struct{}  // index of chunks with Refs == 0
```

- `Commit` increments `Refs` for every hash newly referenced by the inode's chunk list and
  decrements for every hash dropped. `Refs == 0` → `TombstonedAt = cmd.Now`, add to `Tombs`.
- A later `Commit` that references a tombstoned hash revives it (`TombstonedAt = 0`, remove
  from `Tombs`, `Refs = 1`). The flusher's dedup check treats tombstoned chunks as present,
  so revival costs no upload.
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
`GCDelete{Before: now − grace}` with `grace = 10 min`. The FSM deletes every chunk entry with
`Refs == 0 && TombstonedAt < Before` and returns the victims `[]{Hash, Placement}`. The
worker then issues `DELETE` for each victim slot (best effort; misses are caught by
`Sweep`). Because `Before` is in the command, every replica deletes the same entries at the
same log index (I-4).

Why grace: a flusher may have PUT shards for a chunk hash and be about to `Commit`. If the
hash was tombstoned and GC removed the entry in between, the Commit fails with
`ErrUnknownChunk` and the flusher re-uploads (04 §4 step 10). Grace makes that race rare;
correctness does not depend on it.

## 3. Sweep (every node, hourly)

```go
func Sweep(ctx context.Context, s *Store, self meta.NodeIdx, m *meta.Store, grace time.Duration) (deleted int, err error)
```

For every shard file on this node's disk with `mtime < now − grace`:

- parse `key → (hash, idx)`;
- `c, ok := m.GetChunk(hash)`; delete the file if `!ok` **or** `c.Placement[idx] != self`.

A shard younger than grace is never touched (I-14): it may belong to an in-flight flush
whose Commit has not yet been applied locally. `Put` of an already-present shard refreshes
its mtime for the same reason. Sweep never consults the network; it uses only the replicated
chunk table, so a node that returns after a week cleans up exactly what the cluster has
re-placed elsewhere.

## 4. Repair scanner (leader only)

```go
type Repairer struct {
    st *meta.Store; prop meta.Proposer; peers meta.Peers; rd *vfs.Reader
    shards chunk.ShardClient; codecs *ec.Pool; rate *rate.Limiter; clock clock.Clock
}
func (r *Repairer) Scan(ctx context.Context) error   // one pass over ForEachChunk
```

A slot `i` of chunk `c` is **bad** when any of:

1. `c.Placement[i] == 0` (degraded write, or earlier failed re-place);
2. the node is `Removed` or `Draining`;
3. the node has been dead (no successful probe) for longer than `fill_after` (10 min);
4. `(c.K, c.M) != (Vol.K, Vol.M)` and `c.K != 1` — the chunk predates a policy change
   (small chunks with `K == 1` follow `Vol.M` only: bad if `c.M != Vol.M`).

A chunk with ≥ 1 bad slot and ≥ k good live slots is **repairable**. Repair of one chunk:

1. `chunk := rd.ReadChunk(hash)` (verifies the hash; needs k good shards).
2. Determine target `(k', m')`: current policy for case 4, else the chunk's own.
3. `newPlacement := Place(hash, liveCandidates − goodNodes(kept slots), needed)`. For case 4
   every slot is re-placed; for cases 1–3 only the bad slots, keeping good ones in place.
4. `codec(k', m').Encode(chunk)`; PUT the new shards to their nodes; require **all** new
   slots acked (repair is not a client write and may take its time).
5. `Propose(SetPlacement{Hash, K: k', M: m', Placement: newPlacement})`.
6. Only after `WaitApplied`: `DELETE` old shards that are no longer in the placement.

Step ordering guarantees I-15's second clause: at every instant at least k shards decodable
under the *committed* placement exist, because the old placement stays committed until the
new shards are durable, and old shards are deleted only after the new placement is
committed.

Rate limiting: `--rate` (default 50 MB/s of shard traffic) via a token bucket; a scan pass
also yields between chunks so foreground IO is not starved. Progress is exposed as
`admin.RepairStatus{Pending, Repaired, Failed, BytesMoved, PolicyMigrated, PolicyTotal}`.

Chunks with < k good live slots are **unrepairable**; they are counted and listed by
`tailraid check --files` and retried on the next pass (a node may come back).

## 5. Scrub

Every scan pass also verifies a sample of chunks (default 1 % per pass, so a full scrub
completes in ~100 passes; `tailraid check --scrub` forces 100 %): fetch **all** k+m shards,
`codec.Verify`. A `false` result or any `ErrCorrupt` from a node marks the offending slots
bad and repairs them via §4 (the chunk itself is decoded from a hash-verified subset).
Nodes delete a shard on local hash mismatch at read time (I-11), so a corrupt shard also
self-reports as missing.

## 6. Membership flows

**`node drain <n>`** → `NodeSetState{Draining}`. Draining nodes are excluded from
placement candidates immediately; the scanner treats their slots as bad (§4 case 2) and
moves them. `status` shows `draining: 1,234 slots remaining`.

**`node remove <n>`** → refused while the node still holds a slot that is the *only* good
copy of its shard index unless `--force`; otherwise `RemoveServer` (Raft) + `NodeSetState{
Removed}`. Removed nodes keep their `NodeIdx` forever (indexes are never reused) so historical
placements stay interpretable.

**`node add` / `join`** → `NodeUpsert{State: Active}`; the node is a candidate for new
chunks at once. No data moves toward it until the repairer has a reason to (no rebalancing
in barebones; capacity weighting in `Place` biases new writes).

## 7. Policy change: `volume set-redundancy --k K --m M`

1. Refused unless `K + M ≤ live active nodes` and `K ≥ 1, M ≥ 0, K + M ≤ 256`.
2. `SetVolume{K, M, PolicyVersion+1}` is committed. From this log index every new chunk uses
   the new policy (small chunks use `(1, M)`).
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
- **I-14** — A shard file younger than the grace period is never deleted by Sweep or GC, regardless of what the chunk table says.
- **I-15** — Repair restores k+m slots for any chunk with ≥ k good live shards, and migration to a new (k,m) never passes through a state in which fewer than k shards decodable under the committed placement exist.
- **I-41** — A tombstoned chunk re-referenced before GC is revived without re-upload, and its `Refs` is exact.
- **I-42** — Draining a node moves every one of its slots before `node remove` succeeds without `--force`; after removal no committed placement references the node except via `--force`-acknowledged degraded slots.
- **I-43** — `GCDelete` applied on any replica deletes exactly the same chunk entries (determinism via `Before` in the command).
- **I-44** — `fsck` reports zero findings on a cluster where every test in this document passed; any finding is a bug, not noise.

## Tests

| Invariant | Tests |
|---|---|
| I-13 | `TestCluster_GCDoesNotDeleteInflightUpload`, `TestRepair_DeletesOldShardsOnlyAfterCommit`, `TestSweep_KeepsPlacedShards`, `TestModelCheck_Long` |
| I-14 | `TestSweep_RespectsGrace`, `TestGC_RespectsGrace` |
| I-15 | `TestCluster_NodeDiesMidFlush_RepairRestoresRedundancy`, `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK`, `TestCluster_TornShardOnOneNode_ReadDecodes_AndRepairs` |
| I-41 | `TestFSM_RefCount_SharedChunkAcrossFiles`, `TestFSM_TombstoneRevive`, `TestVFS_DedupeIdenticalChunks` |
| I-42 | `TestRepair_DrainMovesAllSlotsBeforeRemove`, `TestCLI_NodeDrain_ThenRemove_MovesSlots` |
| I-43 | `TestFSM_DeterministicAcrossReplicas`, `TestFSM_GCDeleteDeterministic` |
| I-44 | `TestFsck_CleanClusterNoFindings`, `TestFsck_DetectsInjectedRefcountError` |
