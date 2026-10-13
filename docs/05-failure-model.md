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
| Clock | skew between nodes; the FSM never reads a clock and clamps proposer timestamps to be monotonic (02) |

## 2. Crash points

C1–C5 sit inside a single fsync sequence of one file (the WAL or a shard) and are reached
through the `sysfs.FS` seam: the fault filesystem fails or truncates at the exact step and
the test reopens the store. C6–C7 sit between components and are exposed as nil-checked
fields on `vfs.Hooks` so tests can crash the process (or the simulated node) exactly
there. Production leaves every hook nil.

| ID | Point | How a test gets there | Expected outcome after restart |
|---|---|---|---|
| C1 | WAL record written, not yet fsynced | `sysfs` torn write (`TestWAL_ReplayStopsAtTornTail`) | record may be torn; `Replay` discards it; the WRITE was not acknowledged |
| C2 | WAL fsynced, WRITE reply not sent | `sysfs` failed sync then reopen (`TestWAL_AppendAfterFailedSync_NoResurrectedRecord`) | record replayed; client retries the WRITE (idempotent at byte level) |
| C3 | shard temp file written, not fsynced | `sysfs` crash before rename (`TestStore_CrashBeforeRename_LeavesNoVisibleShard`) | temp file swept; no visible shard; flush retries |
| C4 | shard fsynced, not renamed | same seam, `TestStore_TempNeverServed` | temp file swept; flush retries (dedup makes re-PUT cheap) |
| C5 | shard renamed, directory not yet fsynced; no ack sent | `sysfs` crash before `SyncDir` (`TestStore_CrashAfterRenameBeforeDirSync_ShardStillValid`) | the rename may or may not survive power loss; either way no ack was sent, so the sender re-PUTs (idempotent); a surviving file is an orphan shard swept after grace or reused by the re-PUT |
| C6 | all k+m shards acked; Commit not proposed | `Hooks.AfterAllShardsAcked`, `Hooks.BeforeCommit` | orphan shards; swept after grace if never committed (06 §3); flush retries from WAL and re-PUTs (idempotent) |
| C7 | Raft committed, `WaitApplied` not returned / WAL not reset | `Hooks.AfterRaftCommit` | FSM has the new chunk list; WAL still has the extents; the retried flush dedups to the same hashes and commits a no-op (or `ErrConflict` → rebase) |

"Nothing left behind" is part of the rule: after the restart, the retried flush and one
sweep past grace, the gateway's `staging/` is empty, no node holds a `.tmp-*` file under
`shards/` or `cache/`, and no node holds an orphan shard (I-38; `TestCrash_AtEveryPoint`
asserts it at every point).

A leader crash mid-Apply is not a gateway crash point: the entry is either committed on a
majority and replayed, or never committed; `Propose` returns `ErrLeadershipLost` and the
flush retries with `BaseVersion` (02).

Rule (tested by `TestCrash_AtEveryPoint`): after a crash at any point and a restart, exactly
one of two things is true for each write: *not acknowledged* ⇒ the file content equals the
pre-write model and no visible orphan shard exists (temp files swept); *acknowledged* ⇒ the
content equals the post-write model from every live gateway. A third outcome is a bug.

## 3. Survived vs not survived

| Failure | Survived? | Mechanism |
|---|---|---|
| (a) permanent loss of any m targets (disks) | yes | any k of k+m shards reconstruct (I-10); repairer re-places |
| (a′) loss of a whole host with several disks | yes when the policy's host tolerance ≥ 1 (always for an automatic policy on ≥ 2 hosts, I-88) | the host cap keeps ≤ m slots per host (14 §5) |
| (a″) one disk fails or is unmounted while its host runs | yes | the disk goes dead within one probe (I-90), an unmarked mount point is never written (I-86) |
| (b) gateway crash / power loss at any instant incl. right after ack | yes | WAL fsync per record + replay (I-7, I-37) |
| (c) leader crash or partition at any point in Apply | yes | Raft; `BaseVersion` makes retries safe |
| (d) corruption of ≤ m shards per stripe | yes | header hash on read (I-11), decode from others, scrub + repair |
| (e) kill -9 or power loss at any fsync step of a shard write | yes | temp + rename + directory fsync before ack; temps never served |
| (f) loss of a Raft minority | yes | majority keeps committing; minority gateways go read-only |
| (g) > m nodes lost before repair completes | **no** | affected chunks return EIO; everything else continues |
| (h) permanent loss of Raft majority | **no** (operator action) | the survivors cannot elect; `tailraid recover` on one of them rewrites the membership from its own snapshot and log (12 §9), after which every file whose shards survive is readable; fsck-from-shards is out of scope; a mirror (docs/16) restores the volume as of its newest snapshot into a new cluster, or is read with no cluster at all |
| (i) storage that acknowledges fsync without persisting | **no** | outside software control |
| (j) silent corruption of > m shards of one stripe | **no** | detected (EIO), not recoverable |
| (k) destruction of the issuing gateway's own disk before its WAL has flushed | **no** | documented gap (flush happens within 2 s of idle, or at the dirty thresholds while writes continue); WAL mirroring is the future fix (04 §8.2) |

Degraded writes (user's choice, 04 §4 step 9) hold < m parity until repair; during that
window the chunk survives fewer than m additional losses.

## 4. Partition model

- Any subset of links may drop or delay traffic in either direction. Partitions can be
  asymmetric (A reaches B, B does not reach A).
- Raft commits only on the majority side (I-6). The minority side's gateways report
  `Stale` after 30 s without leader contact: metadata mutations return `EROFS`, data WRITEs
  are still accepted into the WAL until the dirty caps.
- Chunk RPC and Raft use separate ports; a partition can affect one and not the other. A
  gateway that can reach the leader but not k storage nodes buffers in the WAL (04 §7).
- Heal: minority nodes catch up by log replay or snapshot install (`Resynced` event); no
  divergent entries exist because the minority never committed.

## 5. Degraded-operation rules (user-visible)

| Condition | Metadata | Reads | Writes |
|---|---|---|---|
| leader reachable, live ≥ k+m | normal | normal | full placement |
| leader reachable, k ≤ live < k+m | normal | OK if k holders live | degraded (≥ k shards, `0` slots); `0` slots are filled as soon as a live spare exists; slots on dead nodes are re-placed after `fill_after` |
| leader reachable, live < k | normal | `NFS3ERR_IO` for affected chunks | flush fails; WAL buffers up to 1 GiB then `NFS3ERR_IO` |
| no leader contact > 30 s (`Stale`) or no quorum (`NoLeader`) | stale local copy | served from local FSM + reachable peers | data WRITEs into the WAL until the caps, then `NFS3ERR_IO`; metadata mutations `EROFS` (`NFS3ERR_ACCES` or `NFS3ERR_IO` on the wire, 07) |

What the user sees:

- `EIO` on READ: fewer than k shards of that chunk are reachable. `tailraid check --files`
  lists the affected paths.
- `EIO` on WRITE: the dirty cap is reached because flushes have been failing for a long
  time. `status` shows WAL depth and the reason (no leader / < k live).
- `EROFS` on create/mkdir/unlink/rename/setattr: the gateway cannot reach a leader. Reads,
  directory listings and writes to existing files keep working (the latter into the WAL).
- Nothing is ever returned as wrong bytes: every read path verifies the chunk hash.
- A holder that stops answering mid-write costs the write at most one straggler grace
  (docs/04 §4 step 9): it is marked suspect and the chunks that follow place around it
  (§6). A persistently slow peer therefore receives fewer hot-path shards and relies on
  the repairer's fill to reach full redundancy.

## 6. Liveness, hold-downs and timings

| Parameter | Default | Meaning |
|---|---|---|
| probe interval | 5 s | all-to-all `GET /health` |
| live threshold | 15 s | a node is *live* if a probe succeeded within this window |
| `Stale` threshold | 30 s | gateway with no leader contact → metadata mutations `EROFS` |
| `fill_after` | 10 min | leader waits this long after a node goes dead before re-placing its slots (laptop sleep); `0` slots have no hold-down |
| `evict_after` | 7 d | `MemberLost` event; `status` marks the node `lost` and its warning names the `node remove --force` command |
| Raft heartbeat / election / lease / commit | 2 s / 2 s / 1 s / 50 ms | — |
| GC grace | 10 min | see 06 |

A laptop that sleeps for 5 minutes: dead after 15 s (excluded from placement for new
chunks), no repair before 10 min, `MemberUp` on return, no data moves. A laptop away for a
week: slots re-placed after 10 min, `MemberLost` at 7 d; its now-unplaced shards are swept
by its own `Sweep` once it has returned, is `Healthy`, and has applied the log past the
re-placements (06 §3).

**Suspect marks.** The window between a node dying and the prober noticing is 15–23 s
(a probe every 5 s, dead after 15 s), during which every chunk written would wait on it
for up to `PutDeadline`. A gateway therefore marks a node *suspect* the moment a shard RPC
to it fails at the transport level (dial refused or timed out, response-header timeout,
the call's own deadline) or a PUT to it outlasts the straggler grace (docs/04 §4 step 9):
`Live()` excludes a suspect host and its disks, so neither placement nor a read dials it,
until the first probe of it that succeeds after the mark (at most one probe interval for a
false positive). A reply with any HTTP status, and a request the gateway cancelled for its
own reasons (a losing hedge, a decided chunk), never mark. A mark changes placement and
reads only: the hold-downs above (`fill_after`, `evict_after`) and repair are untouched.
`status` shows `suspect` on the node while the mark holds (I-170, L-210).

## 7. Clock assumptions

- The FSM never reads a clock; every command carries `Now` from the proposer, so replay is
  deterministic (I-4). The FSM clamps every recorded timestamp to `max(cmd.Now,
  State.LastNow)` (02), so the FSM clock never goes backwards whichever proposer's clock is
  behind. Timestamps are advisory (mtime, tombstone age).
- Skew between devices affects only GC grace and hold-downs, both of which have wide
  margins (minutes). A gateway whose clock is hours *behind* cannot back-date a tombstone
  because of the clamp; one hours *ahead* only delays GC of its own tombstones, which is
  safe. `GCDelete{Before}` is proposed by the leader against the FSM clock and applied
  identically everywhere.
- Raft uses its own real-time timers; tests shorten them rather than faking them.

## 8. Disk-full and capacity

- A node whose chunk store would exceed its configured capacity returns `ErrFull` → RPC
  `507`. The flusher treats it like a failed slot (re-place once, else record 0).
- `/health` carries free bytes; the gateway doing a flush excludes nodes with < 1 shard of
  headroom, and nodes that answered `507` recently, from its own candidate list (03).
- Each added disk has its own budget and its own `507` (14 §3); a full disk does not stop
  its host's other targets.
- WAL and raft.db live on the device's own disk; if that disk is full, WRITE returns
  `NFS3ERR_NOSPC` (the WAL append failed before any acknowledgement, so nothing is lost).

## 9. Data-loss tests

- `TestCrash_AtEveryPoint` — iterate C1..C7; crash the gateway there; restart; assert the
  two-outcome rule of §2 for every write and no visible orphan shard.
- `TestDurability_AckImpliesReadableFromKSurvivors` — real dirs and raft-boltdb; after each
  acknowledged write kill the gateway, restart it, let its WAL replay and flush (quiesce),
  then kill m other nodes and read back via a surviving gateway; rotate the gateway node;
  20 iterations under `-short`, 200 in `make test-long`. (Before the flush the bytes live
  only in the issuing gateway's WAL, by contract.)
- `TestCluster_KillMNodes_AllDataReadable_KillMPlusOne_EIO`.
- `TestRepair_ReportedBadSlot_AndScrub_RepairCorruption`.
- `TestCluster_SplitBrain_OnlyMajorityCommits`, `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`.
- `TestModelCheck_Long` — 5 nodes 3+2, weighted scheduler of client ops, `Kill`, `Restart`,
  `Partition`, `Heal`, `Clock.Advance`, `Scrub`; every 50 steps heal + quiesce and assert:
  every acknowledged byte readable from every live gateway (I-12); every chunk referenced by
  committed metadata has its shards on disk and, after repair converges, on k+m distinct
  nodes (I-9, I-13); no visible shard fails its hash (I-11); all FSMs identical (I-4).
  Seeded; `-replay <seed>` reproduces.
- Soak suite phases 4–6 and 9 (see 09) repeat the above on real processes and disks.

## Invariants

- **I-12** — Every acknowledged byte is readable, unchanged, from the issuing gateway immediately and from any live gateway once flushed, after the loss of any m nodes, provided the failures are within the contract of §3.
- **I-38** — For every write, after a crash at any point C1..C7 and a restart, exactly one holds: the write was not acknowledged and the file equals its pre-write content with no visible orphan shard, or it was acknowledged and the file equals its post-write content on every live gateway.
- **I-39** — A gateway that cannot reach a Raft leader for longer than the `Stale` threshold refuses metadata mutations with `EROFS`, keeps accepting data WRITEs into its WAL until the dirty caps, and continues to serve reads from its local FSM.
- **I-40** — No read path returns bytes that do not hash to the chunk's recorded hash,
  except that a range within one data shard may be returned once that shard hashes to
  the shard hash recorded with the chunk in Raft (`Chunk.Sums`, I-201), before the
  chunk's own check; a chunk enters the cache only after its hash check; on shortage of
  shards every read path returns `EIO`.
- **I-202** — `Reader.ReadAtEarly` returns a range as soon as every data shard it covers
  arrived and hashed to its `Sums[j]`, and otherwise (a covered shard whose holder is
  dead, whose GET failed or whose sum did not match, a chunk without sums) after the
  chunk's own hash check as `ReadAt` does, bytes at or past the chunk's size being zero; a shard that fails its sum is a bad slot
  and the fetch moves on; a chunk whose decode fails its hash fails every reader still
  waiting with `EIO`, records E-134 and never enters the cache; `ReadAt`, `ReadChunk` and
  `BaseReader` never return early and are what the flusher, repair, scrub and the pins
  worker use; `status` counts the ranges served early.
- **I-170** — A node whose shard RPC failed at the transport level, or whose PUT outlasted
  the straggler grace, is excluded from `Live()` (with its disks) until the first probe
  that succeeds after the mark; an HTTP-status reply or a request the gateway itself
  cancelled never marks a node, and a mark never affects the hold-downs or repair.
- **I-246** — A shard GET or PUT (and a mirror GET) queues for its slots at most
  `SlotWait` (3 s, well inside the caller's 10 s header timeout) and is then answered
  `503` with `X-Tailraid-Busy`, which the caller sees as `ErrBusy` wrapped in
  `ErrUnavailable` (never `ErrNoLeader`) and answers by moving to another slot; a busy
  reply is an HTTP status, so it never marks the node suspect (I-170). Before, a request
  queued until the caller's own deadline fired at the transport level, and a node that
  was only busy was marked suspect and routed around.

## Tests

| Invariant | Tests |
|---|---|
| I-12 | `TestCluster_KillMNodes_AllDataReadable_KillMPlusOne_EIO`, `TestCluster_Policy1Plus0_WriteRestartRead_CorruptIsEIO`, `TestCluster_StaleGatewayOverlay_KeepsOtherGatewaysBytes`, `TestCrash_AtEveryPoint`, `TestDurability_AckImpliesReadableFromKSurvivors`, `TestFlusher_DedupHit_LiveButMissingShards_RePuts`, `TestModelCheck_Long` |
| I-38 | `TestCrash_AtEveryPoint` |
| I-39 | `TestCluster_IsolatedGateway_ReturnsEROFS_ReadsStillServed`, `TestNFS_ReadOnly_WireStatuses`, `TestVFS_ProposeFails_RetriesUntilStale`, `TestStaleness_Table` |
| I-40 | `TestCluster_KillMNodes_AllDataReadable_KillMPlusOne_EIO`, `TestCluster_Policy1Plus0_WriteRestartRead_CorruptIsEIO`, `TestReader_CorruptShard_DecodesFromOthers_AndReports`, `TestReader_DecodedHashMismatch_EIO`, `TestReader_FewerThanKLive_EIO`, `TestReader_ReadRange_ChunkHashMismatch_LateReadersEIO_EventRecorded` |
| I-202 | `TestPhaseHotPath_ConformingGatewayPasses`, `TestReader_ReadRange_ServedFromDataShardBeforeDecode`, `TestReader_ReadRange_StraddlesTwoDataShards`, `TestReader_ReadRange_CorruptDataShard_WaitsForDecode`, `TestReader_ReadRange_DeadDataHolder_WaitsForDecode`, `TestReader_ReadRange_NoSums_WholeChunk`, `TestReader_ReadRange_HolesAndShortChunk_Table`, `TestReader_ReadRange_ChunkHashMismatch_LateReadersEIO_EventRecorded`, `TestReader_FlusherBaseRead_NeverEarly`, `FuzzReadRange`, `TestCluster_EarlyServe_HolderKilledMidRead_StillExact`, `TestCluster_EarlyRange_SameBytesEveryGateway` |
| I-246 | `TestServer_BusySlots_AnswerBusy_NotSuspect` |
| I-170 | `TestProber_SuspectUntilNextProbe`, `TestClient_TransportFailureMarksSuspect_Table`, `TestFlusher_SuspectNodeExcludedFromNextChunk` |
