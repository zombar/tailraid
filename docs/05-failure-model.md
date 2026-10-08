# 05 — Failure model

This document enumerates the failures tailraid is designed to survive, the ones it is not,
the exact points at which a crash can happen, and what the user observes in each degraded
state. Every claim here is backed by a named test in §9.

## 1. Components and what can fail

| Component | Failure modes considered |
|---|---|
| Gateway process (vfs + nfsgw + s3srv on a device) | crash, kill -9, power loss, restart with dirty WAL |
| Storage node (chunk store on a device) | process crash, disk full, disk destroyed, bit rot, truncated or zero-filled shard files, lost rename |
| Raft voter / non-voter | crash, long absence (days), partition, IP change |
| Network (tailnet) | partition, asymmetric reachability, latency spikes, dropped connections |
| Clock | skew between nodes; the FSM never reads a clock |

## 2. Crash points

Each point is exposed as a nil-checked field on `cluster.Hooks` so tests can crash the
process (or the simulated node) exactly there. Production leaves every hook nil.

| ID | Point | Hook | Expected outcome after restart |
|---|---|---|---|
| C1 | WAL record written, not yet fsynced | `BeforeWALSync` | record may be torn; `Replay` discards it; the WRITE was not acknowledged |
| C2 | WAL fsynced, WRITE reply not sent | `BeforeClientAck` | record replayed; client retries the WRITE (idempotent at byte level) |
| C3 | shard temp file written, not fsynced | `AfterShardTempWritten` | temp file swept; no visible shard; flush retries |
| C4 | shard fsynced, not renamed | `AfterShardFsync` | temp file swept; flush retries (dedup makes re-PUT cheap) |
| C5 | shard renamed; Commit not proposed | `AfterShardRename` / `AfterAllShardsAcked` | orphan shard; swept after grace if never committed (06 §3); flush retries from WAL |
| C6 | Raft committed, `WaitApplied` not returned | `AfterRaftCommit` | FSM has the new chunk list; WAL still has the extents; the retried flush dedups to the same hashes and commits a no-op (or `ErrConflict` → rebase) |
| C7 | leader crash mid-Apply | (raft) | entry either committed on a majority and replayed, or never committed; `Propose` returns an error; flush retries with `BaseVersion` |

Rule (tested by `TestCrash_AtEveryPoint`): after a crash at any point and a restart, exactly
one of two things is true for each write: *not acknowledged* ⇒ the file content equals the
pre-write model and no visible orphan shard exists (temp files swept); *acknowledged* ⇒ the
content equals the post-write model from every live gateway. A third outcome is a bug.

## 3. Survived vs not survived

| Failure | Survived? | Mechanism |
|---|---|---|
| (a) permanent loss of any m nodes' disks | yes | any k of k+m shards reconstruct (I-10); repairer re-places |
| (b) gateway crash / power loss at any instant incl. right after ack | yes | WAL fsync per record + replay (I-7, I-37) |
| (c) leader crash or partition at any point in Apply | yes | Raft; `BaseVersion` makes retries safe |
| (d) corruption of ≤ m shards per stripe | yes | header hash on read (I-11), decode from others, scrub + repair |
| (e) kill -9 at any fsync step of a shard write | yes | temp + rename; temps never served |
| (f) loss of a Raft minority | yes | majority keeps committing; minority gateways go read-only |
| (g) > m nodes lost before repair completes | **no** | affected chunks return EIO; everything else continues |
| (h) permanent loss of Raft majority | **no** | metadata gone; shards remain but are not addressable (fsck-from-shards is out of scope) |
| (i) storage that acknowledges fsync without persisting | **no** | outside software control |
| (j) silent corruption of > m shards of one stripe | **no** | detected (EIO), not recoverable |
| (k) destruction of the gateway's own disk within the ≤ 2 s WAL→flush window | **no** | documented gap; WAL mirroring is the future fix (04 §8.2) |

Degraded writes (user's choice, 04 §4 step 9) hold < m parity until repair; during that
window the chunk survives fewer than m additional losses.

## 4. Partition model

- Any subset of links may drop or delay traffic in either direction. Partitions can be
  asymmetric (A reaches B, B does not reach A).
- Raft commits only on the majority side (I-6). The minority side's gateways report
  `Stale` after 30 s without leader contact and refuse new writes with `EROFS`.
- Chunk RPC and Raft use separate ports; a partition can affect one and not the other. A
  gateway that can reach the leader but not k storage nodes buffers in the WAL (04 §7).
- Heal: minority nodes catch up by log replay or snapshot install (`Resynced` event); no
  divergent entries exist because the minority never committed.

## 5. Degraded-operation rules (user-visible)

| Condition | Metadata | Reads | Writes |
|---|---|---|---|
| leader reachable, live ≥ k+m | normal | normal | full placement |
| leader reachable, k ≤ live < k+m | normal | OK if k holders live | degraded (≥ k shards), repaired after `fill_after` |
| leader reachable, live < k | normal | EIO for affected chunks | flush fails; WAL buffers up to 1 GiB then `NFS3ERR_IO` |
| no leader contact > 30 s (`Stale`) or no quorum (`NoLeader`) | stale local copy | served from local FSM + reachable peers | `EROFS` |

What the user sees:

- `EIO` on READ: fewer than k shards of that chunk are reachable. `tailraid check --files`
  lists the affected paths.
- `EIO` on WRITE: the dirty cap is reached because flushes have been failing for a long
  time. `status` shows WAL depth and the reason (no leader / < k live).
- `EROFS`: the gateway cannot reach a leader. Reads and directory listings keep working.
- Nothing is ever returned as wrong bytes: every read path verifies the chunk hash.

## 6. Liveness, hold-downs and timings

| Parameter | Default | Meaning |
|---|---|---|
| probe interval | 5 s | all-to-all `GET /health` |
| live threshold | 15 s | a node is *live* if a probe succeeded within this window |
| `Stale` threshold | 30 s | gateway with no leader contact → writes `EROFS` |
| `fill_after` | 10 min | leader waits this long after a node goes dead before re-placing its slots (laptop sleep) |
| `evict_after` | 7 d | `MemberLost` event; `status` suggests `node remove` |
| Raft heartbeat / election / lease / commit | 2 s / 2 s / 1 s / 50 ms | — |
| GC grace | 10 min | see 06 |

A laptop that sleeps for 5 minutes: dead after 15 s (excluded from placement for new
chunks), no repair before 10 min, `MemberUp` on return, no data moves. A laptop away for a
week: slots re-placed after 10 min, `MemberLost` at 7 d; its shards become orphans swept
by its own `Sweep` when it returns (06 §3).

## 7. Clock assumptions

- The FSM never reads a clock; every command carries `Now` from the proposer, so replay is
  deterministic (I-4). Timestamps are advisory (mtime, tombstone age).
- Skew between devices affects only GC grace and hold-downs, both of which have wide
  margins (minutes). A node whose clock is hours off cannot delete data early because
  `GCDelete{Before}` is decided by the leader's clock and applied identically everywhere.
- Raft uses its own real-time timers; tests shorten them rather than faking them.

## 8. Disk-full and capacity

- A node whose chunk store would exceed its configured capacity returns `ErrFull` → RPC
  `507`. The flusher treats it like a failed slot (re-place once, else record 0).
- `/health` carries free bytes; the leader excludes nodes with < 1 shard of headroom from
  placement candidates.
- WAL and raft.db live on the device's own disk; if that disk is full, WRITE returns
  `NFS3ERR_NOSPC` (the WAL append failed before any acknowledgement, so nothing is lost).

## 9. Data-loss tests

- `TestCrash_AtEveryPoint` — iterate C1..C7; crash the gateway there; restart; assert the
  two-outcome rule of §2 for every write and no visible orphan shard.
- `TestDurability_AckImpliesReadableFromKSurvivors` — real dirs and raft-boltdb; after each
  acknowledged write kill the gateway and m other nodes; read back via a surviving gateway;
  rotate the gateway node; 20 iterations under `-short`, 200 in `make test-long`.
- `TestCluster_KillMNodes_AllDataReadable`, `TestCluster_KillMPlusOne_ReadsFailCleanly`.
- `TestCluster_TornShardOnOneNode_ReadDecodes_AndRepairs`.
- `TestCluster_SplitBrain_OnlyMajorityCommits`, `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`.
- `TestModelCheck_Long` — 5 nodes 3+2, weighted scheduler of client ops, `Kill`, `Restart`,
  `Partition`, `Heal`, `Clock.Advance`, `Scrub`; every 50 steps heal + quiesce and assert:
  every acknowledged byte readable from every live gateway (I-12); every chunk referenced by
  committed metadata has its shards on disk and, after repair converges, on k+m distinct
  nodes (I-9, I-13); no visible shard fails its hash (I-11); all FSMs identical (I-4).
  Seeded; `-replay <seed>` reproduces.
- Soak suite phases 4–6 and 9 (see 09) repeat the above on real processes and disks.

## Invariants

- **I-12** — Every acknowledged byte is readable, unchanged, from any live gateway after the loss of any m nodes, provided the failures are within the contract of §3.
- **I-38** — For every write, after a crash at any point C1..C7 and a restart, exactly one holds: the write was not acknowledged and the file equals its pre-write content with no visible orphan shard, or it was acknowledged and the file equals its post-write content on every live gateway.
- **I-39** — A gateway that cannot reach a Raft leader for longer than the `Stale` threshold refuses new writes with `EROFS` and continues to serve reads from its local FSM.
- **I-40** — No read path ever returns bytes that do not hash to the chunk's recorded hash; on shortage of shards it returns `EIO`.

## Tests

| Invariant | Tests |
|---|---|
| I-12 | `TestDurability_AckImpliesReadableFromKSurvivors`, `TestCluster_KillMNodes_AllDataReadable`, `TestModelCheck_Long`, `TestE2E_ThreeNodes_NFSWriteOnA_ReadOnB_KillC` |
| I-38 | `TestCrash_AtEveryPoint`, `TestOverlay_RestartWithDirtyBuffer` |
| I-39 | `TestCluster_StaleGatewayReturnsEROFS`, `TestCluster_QuorumLostReadsStillServed` |
| I-40 | `TestReader_CorruptShardReconstructs`, `TestReader_TooManyCorruptShardsEIO`, `TestCluster_KillMPlusOne_ReadsFailCleanly` |
