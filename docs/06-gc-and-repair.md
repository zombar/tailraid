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
- `Unlink`, `Truncate` (via `Commit`) and `Purge` are the only paths that decrement. A
  delete does not: it is a `Trash` (02 §Apply rules), which moves the entry out of the
  namespace and keeps the inode with every reference it holds, so nothing a deleted
  file used is reclaimed until the entry is purged, by the user ("delete now") or by the
  retention purge of §2. The same holds for the entry a `Rename` replaces. The only
  entries that are unlinked outright are the **ephemeral names**: an upload's temp
  (`.<name>.<node>.tailraid-upload`, any name ending `.tailraid-upload`), everything under `/.tailraid` (the thumbnails of 13 §5 and
  the stats segments of 17 §2: a tree the node writes for itself, never the user; I-195)
  and the files macOS writes beside the user's over NFS (`._<name>`, `.DS_Store`);
  `meta.IsEphemeral` is the one list, used by the FSM's `Rename`, the vfs and the web UI
  (I-157, L-208).
- A file that is both in the trash and still referenced from a live file (dedup) costs
  nothing extra; a file whose chunks are referenced only from the trash keeps them
  committed and placed, repaired and scrubbed like any other (L-199).
- Invariant checked by `fsck` and by the FSM test suite: `Σ Refs == number of (inode,
  index) pairs referencing the hash`, counting the files in the trash, and
  `Tombs == {h : Refs == 0}`.

## 2. GC worker (leader only)

```go
type GC struct { st *meta.Store; prop meta.Proposer; shards chunk.ShardClient; grace time.Duration; clock clock.Clock }
func (g *GC) RunOnce(ctx context.Context) (deleted int, err error)
```

Every `gc_interval` (default 10 min) the leader proposes
`GCDelete{Before: fsmNow − grace, Limit: 1024}` (and again while a batch comes back
full, I-103: after an `rm -rf` of terabytes the FSM lock is held for 1024 entries at a
time, never for all of them) with `grace = 10 min`, where `fsmNow = max(leader wall
clock, State.LastNow)`: the leader's clock clamped by the FSM clock (02), the same clock that
stamped every `TombstonedAt`. A leader whose clock lags the proposers therefore never stalls
GC, and `Before` never goes backwards across leader changes. A leader whose clock runs ahead
by Δ shortens grace by Δ (bounded by the skew assumption in 05 §7); the only consequence is
that a flusher mid-dedup gets `ErrUnknownChunk` and re-uploads, never data loss. The FSM
deletes every chunk entry with `Refs == 0 &&
TombstonedAt < Before` and returns the victims `[]{Hash, K, M, Placement}`. The worker
then issues `DELETE /v1/disks/{id}/shards/{key}` for each victim slot on **disk** rows with the
header `X-Tailraid-Log-Index: <index of the GCDelete>` (best effort; misses are caught by
`Sweep`). Because `Before` is in the command, every replica deletes the same entries at the
same log index (I-4).

**DELETE fence (devices and disks).** The receiver deletes the file only if all of:
1. its own applied Raft index ≥ `X-Tailraid-Log-Index` (it has seen the GCDelete or
   SetPlacement that justifies the delete);
2. its chunk table has no entry for the hash, or the entry's `Placement[idx] != self` under
   that key's `(k,m)`;
3. the file's mtime is older than grace (`Store.DeleteIfOlder`). One rule everywhere a
   shard or an upload temp is aged (Sweep, GC, repair, the upload sweep, the FSM's
   tombstones): younger than grace *or exactly grace old* is kept; one tick older goes.
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

**Retention purge.** On the same tick, before `GCDelete`, the leader proposes
`Purge{Before: fsmNow − retention, Limit: 1024}` and again while `TrashDue(Before)` is
still non-zero and the last entry made progress, with `retention = trash.retention` (12
§10; 30 days unless set; `0` means the trash is never purged on its own, only by the
user). `fsmNow` is the same clamped clock as above, so a leader with a slow clock never
keeps a deleted entry past its day and `Before` never goes backwards across leader
changes. The purge drops the references of every file it deletes; the chunks that reach
zero are tombstoned at that moment and collected by `GCDelete` after grace, so a 30-day
old delete is reclaimed about ten minutes after its day and a "delete now" from the
browser UI the same way (I-157). Nothing in the trash is ever a GC victim while it is in
the trash (L-207).

**The sync hold** (16 §4). While a snapshot is being copied to a mirror, its catalogue row
is `syncing`, and the FSM clamps every `GCDelete` to `Before = min(cmd.Before, the oldest
syncing row's At)`: a chunk the snapshot references that loses its last reference during
the copy is tombstoned as usual but not collected until the row ends, so the copy never
meets a hole (I-181, L-212). The clamp is applied by the FSM from its own state, so every
replica deletes the same entries (I-43). Nothing else changes for the pool: the purge
still tombstones, the sweep still removes only unplaced shards, and the hold lasts at most
`mirrors.hold_max`.

**Mirrors.** Each mirror's schedule (16 §4) runs on its own host's minute tick, since only
the host knows whether the disk is attached: for each online mirror of the host with no
sync running, a `SnapshotBegin` named by the UTC minute when its newest automatic snapshot
is older than `mirrors.every`, at once after an offline stretch; nothing for an offline
one. Per mirror, `SnapshotDelete` for every automatic snapshot outside the keep set (the
newest of each of the last `keep.hourly`, `keep.daily`, `keep.weekly` and `keep.monthly`
buckets), oldest first, and the oldest kept automatic one too while the mirror's free
space is under its reserve; a manual snapshot is never deleted. The host also ends a sync
of its own that lost its clone, and cleans its disk (16 §5). The leader's one part, on
its own minute tick: a `SnapshotEnd{OK: false}` for any sync older than
`mirrors.hold_max`, whose host may be gone. Nothing is proposed while a live member runs
a codec older than 7 (I-182).

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

The hourly tick also runs the namespace sweeps of the web UI (13): upload temps idle for
24 h are removed (I-81), and so is every thumbnail under `/.tailraid/thumbs` whose
`<ino>-<gen>-<version>` names no live file at that version (the file changed, was
purged, or never existed), through `Unlink` since the names are ephemeral (I-160), and
every video frame grabbed by an older recipe (13 §5, I-216). A
thumbnail of a file in the trash is kept: the browser UI shows the deleted file's tile.
On the leader the same tick removes every search segment under `/.tailraid/index` that
the current chain does not name once it is an hour old (18 §4, I-206): a rebuild's
predecessors and an abandoned pass's temp; a segment in the chain is never touched.

**S3 sweep (leader only, daily).** For each S3 node the leader lists the bucket prefix with
`ListObjectsV2` (paged) and deletes every object whose `LastModified` is older than grace
and whose key is not placed on that node in the chunk table (I-138). Only the leader runs it,
only while `Healthy`, and only for a bucket it holds the credentials of; it checks on the
hourly sweep tick and sweeps a bucket once a day. The repairer and the GC worker never
send a DELETE to a bucket (10 §4.2): a superseded or collected object stays until this
sweep finds it. A bucket has no conditional delete, so placement is checked once more just
before each DELETE; an upload of the same shard that lands between that check and the
DELETE (a re-write of content collected over a day ago, in the same second) leaves one
slot whose object is missing, which the repairer's HEAD finds and refills (L-112). Because a dup PUT to S3 overwrites (fresh `LastModified`), an object a
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
   or a stale shard gets repaired without waiting for the scrub to reach it;
6. the chunk is **skewed** (14 §7): one host holds more than `ceil((k+m)/D)` of its slots
   while the candidates admit a placement within that cap; the excess slots (highest
   index first) are bad and move like draining slots, each to its home target when that
   target can take it (I-296);
7. after the per-chunk pass, while the **home gate** is open (14 §7, I-295), slots that
   are away from their home for placement only (a data slot on a non-voter, the wrong disk
   of a host, a host that is not home since a target joined) move to their home, at most
   `HomeBatch` chunks a pass and none when the pass before found a bad slot or a pending
   migration, as readable bad slots (reason `away from home`) through the repair of one
   chunk below, which sends them home or leaves them where they are. This one
   rule replaces the separate capacity rebalance within a host of earlier releases
   (I-172).

A chunk with ≥ 1 bad slot and ≥ k good live slots is **repairable**. Repair of one chunk:

1. `chunk := rd.ReadChunk(hash)` (verifies the hash; needs k good shards).
2. Determine target `(k', m')`: current policy for case 4, else the chunk's own.
3. `newPlacement`: each refilled slot goes to its home target (14 §7) when that target
   holds no slot of the chunk and is live, not suspect and has room (I-296); the rest
   `FillPlacement(hash, liveCandidates − goodNodes(kept slots))`, counting the domains of
   the kept slots against the host cap (I-89). Slots no one serves are filled before
   slots whose holder still serves them (draining, skewed, away): their holders stand in
   while the others are filled, so a lone spare replaces a missing shard first (I-65); an
   away slot goes home or keeps its holder; a PUT that fails leaves a still-served slot on
   its holder. For case 4
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

**Pacing.** A pass works on at most `repair.workers` chunks at once (default 4, at most
64; I-174): each chunk's steps stay in sequence, chunks overlap, so the pass is bounded
by its targets' latency times the fan-out rather than by one round trip per step, and
the statistics are sums, the same whatever the overlap; a cancelled pass starts no more
chunks and returns once the ones in flight end. Every shard request of a pass (its
reads, PUTs, HEADs and DELETEs, the scrub's and the rebalance's too) carries the
background priority (`X-Tailraid-Priority: background`, [08](08-security.md)), and a node
serving it lets background requests hold at most half of its PUT and, separately, GET
slots ([04 §9](04-write-path-and-durability.md)), so a client's reads and flushes always
find the other half free; a background request queues for its share, is never refused,
and gets the same reply (I-175). Shard bytes are also metered by a token bucket of
`repair.rate` a second (default 50 MiB, `0` for no limit), total across the workers.
Progress is `admin.RepairStatus`: the last completed pass's `stats`, and while a pass
runs `in_progress` (its number, start, chunks scanned of total, chunks in flight and
workers, bytes moved; [12 §8](12-operations.md)), which `status` prints as one line and
`check` carries, so a long first pass is never read as a stall.

Chunks with < k good live slots are **unrepairable**; they are counted and listed by
`tailraid check --files` and retried on the next pass (a node may come back). Both repair
and the GC worker are leader-only and run only while the leader is `Healthy`.

**Sums backfill (04 §4, I-201).** A chunk committed before codec 5, or in a mixed
cluster, carries no `Sums`, so the read path serves it whole. Each pass takes at most
`SumsBatch` (256) such chunks, in hash order, among those it found healthy: it fetches k
shards (background priority), decodes, verifies the chunk's hash, re-encodes and hashes
the k data shards, and proposes `SetShardSums`; a chunk that fails its hash is left for
the scrub. `status` reports `sums_recorded` and `sums_pending` in the last pass's stats;
the backfill is complete when the latter reads 0. A cluster written on an older release is covered at the pass rate: 256 chunks a pass, a pass about every four minutes while the backfill reads 1 GiB per pass at 4 MiB chunks (110k chunks took about 30 hours on the real cluster).

## 4a. Where a pass looks: placement classes and the sweep

Until 0.10.4 every pass, a minute after the last, copied every live chunk, sorted them
and derived each one's state and home: work in proportion to the volume, all day, on a
healthy cluster (the leader of the real cluster sat at a core and more), and `status`
walked the same table every 2 s while anyone watched (12 §8). Neither looks at a chunk
that needs nothing now.

**Placement classes (I-377).** A chunk's state without shard reports (its bad slots,
readable count, pending migration and host skew, docs/11 §2) depends only on its policy,
its slots as a set, whether it is a small `k = 1` chunk, the node table and liveness. The
FSM therefore keeps the live chunks grouped by **class**, the key `(k, m, small, sorted
slots)`, each with its chunk count, its shard bytes (what each slot of the class holds)
and its members. The applier keeps the table in step with every command at the same
points it keeps the volume totals (a chunk counted live, uncounted, or re-placed), and
`Restore` rebuilds it; it is never encoded, and a recount gives the same table (the
model checker compares after every step). The number of classes is the number of slot
sets in use, which the targets bound (at most `C(targets, k+m)` plus the degraded sets),
not the volume: 21 on seven targets at `3+2`.

**Status from classes (I-378).** `ScanHealth` judges each class once by a representative
chunk (its policy, its slots, a size with its small bit) and counts the result for all
its chunks: degraded, repair-pending, unreadable, policy-stale and host skew, and each
node's slots, shard bytes and missing slots. `check --files` lists the files of the
members of the classes in those states and nothing else. The disk skew (away slots whose
home is another disk of their host) depends on each chunk's hash, so it is the leader's
last sweep's count (`repair.stats.disk_skew`); a follower's report leaves it at 0.

**Targeted passes and the sweep (I-379).** A pass judges the classes and visits only:
the members of classes with a bad slot or a pending migration (cases 1–4 and 6 of §4),
the chunks with a reported slot (case 5), the next `HomeBatch` chunks of the home queue
while the gate is open (case 7), and the next `SumsBatch` chunks of the sums queue. The
**sweep** is a pass that visits every chunk as passes did before: the first pass as
leader, the first after `SweepInterval` (1 h) since the last sweep, a pass whose home
candidate set changed, `check --scrub`, a pass while the gate is open and its queue is
empty, and the first pass after repairs re-placed chunks once no class needs work (a
refill can leave a slot away from home, which only a sweep counts). One more trigger is a
pass where a target can take shards that could not at the last sweep, when that sweep
left away bytes it could not move. That covers a host back from the dead, or a full disk
with room again. A refill while a home was dead leaves its slots away and unmovable until
that home returns. Without this trigger they would wait for the hourly sweep; the per-pass
walk before 0.10.4 brought them home within minutes (`make soak` phase 11 on 2026-10-16
found the corpus's data slots on a non-voter after phase 6 restarted the leader). A host
that flaps costs one sweep each time it returns. A sweep scrubs the
chunks whose turn came since the previous sweep (§5), counts the slots away from home,
the movable bytes and the disk skew (between sweeps `status` shows these from the last
one), and queues in hash order the movable chunks it did not move and the chunks still
without sums, at most `QueueMax` (2^20) each, for the passes after it. On an idle volume
a pass costs the number of classes; a sweep costs one walk an hour. A test rig sets
`SweepInterval` to 0 to sweep every pass.

**Stuck classes (I-380).** A class whose chunks a pass visited and could do nothing for,
with no PUT tried (no live spare outside the kept slots, fewer than k readable slots), is
**stuck** under the view it was judged in: each member's state, liveness and whether it
can take a shard now (not held slow, not marked full), and the volume's policy. The
passes after it skip the class until that view changes (a target joins or comes back, a
hold ends), `StuckRetry` (10 min) passes, or a sweep visits it. A failed attempt (a PUT
refused or cut, a read that failed, a migration short of k acks, a commit that lost a
race) is retried on the next pass as before. Without this, a dead disk with no spare
would have every pass visit every chunk on it for nothing.

## 5. Scrub

Every sweep (§4a) also verifies the chunks whose turn has come, so that the background
sweeps verify the whole volume once per `repair.scrub_period` (default `168h`, a week; `0` turns
the background scrub off; `tailraid check --scrub` runs one 100 % pass through the leader's
repairer, with the configured `fill_after` and rate limit, serialized with the background
passes). The turn is a function of time, not of the pass count: chunk `c` has the phase
`φ(c) = BigEndian(c.Hash[0:8]) mod P` (P the period in nanoseconds), and a sweep starting
at `t` scrubs the chunks whose phase lies in the arc `(t₀ mod P, t mod P]`, where `t₀` is
the start of the repairer's previous sweep, or `t − SweepInterval` on its first sweep as
leader; an arc of P or more covers every chunk, and the passes between sweeps scrub
nothing (I-215). The cost is therefore stored bytes
÷ P whatever the pass rate and the volume size, and nothing has to survive a leader
change. (Until 0.5.2 the sample was 1 % of the chunks per pass: a pass starts a minute
after the last one ends, so a 720 GB volume was re-read every 6–7 hours, ~2.5 TB a day of
disk reads and tailnet traffic on an idle cluster, measured on the real cluster on
2026-10-12.) A scrubbed chunk's verification is to
fetch **all** k+m shards,
`codec.Verify`. A `false` result or any `ErrCorrupt` from a node marks the offending slots
bad and repairs them via §4 (the chunk itself is decoded from a hash-verified subset).
A slot on an S3 node is checked with a HEAD, and an absent object is a bad slot (L-112,
L-139); fetching every one would download the bucket's share of the volume about every
hundred passes in billed egress, and its bytes are verified by the provider on upload
(`x-amz-content-sha256`) and by the TRS1 header on every GET (10 §4.2). Because the bucket
is the copy a degraded read falls back on, the scrub also fetches bucket objects within a
daily byte budget, `repair.s3_scrub` (default 1 GiB a day, `0` for HEAD only), and verifies
them exactly like disk shards: the budget refills continuously and the scrub fetches a slot
while it is in credit, so a corrupt object is found over time at a bounded egress cost and
re-PUT by the repair below (I-151, L-198).
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
chunks at once. Data moves toward it only through home placement (14 §7): the chunks whose home now
includes the new target, about its weight over the total, become away, cap-breaking ones
move at once (the host cap, I-296), the rest once the home gate opens (I-295). `node add-disk` is a join of a target.

## 7. Policy change: automatic, or `volume set-redundancy --k K --m M`

In auto mode (the default from `init`) the leader proposes `SetVolume` at the start of a
repair pass when the rule of 03 gives a different policy than the volume has; the steps
below are the same as for a manual change.

1. Refused unless `K + M ≤ live Active targets (devices, disks or S3)` and `K ≥ 1, M ≥ 0, K + M ≤
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

`tailraid chunk describe <hash|/path>` answers from the node asked (no forwarding): the
state derived from its FSM copy and liveness (`committed`, `degraded`, `unreadable`,
`tombstoned`, `zero`, `unknown`), `(k,m)` with a policy-stale flag, refs, every slot with
its node, liveness and a `HEAD` of the shard on live nodes, the referencing files, and the
node's chunk event ring (docs/11 §7; `(event ring disabled)` unless `dev.enabled`). A hash
prefix of at least 8 hex digits is accepted when it is unique; a path describes every chunk
of that file in order.

`tailraid fsck` checks, from the asked node's FSM copy, every FSM invariant
(`meta.CheckInvariants`): refcount sums (the files in the trash included); `len(Chunks) ==
ceil(Size/CS)` for every file; every parent chain reaches root or a deleted root and every
`Children` entry points to an inode whose `Parent`/`Name` agree; `Tombs ⇔ Refs == 0`;
`Trash ⇔ DeletedAt ≠ 0`, a deleted root absent from its parent's `Children`, a
directory's `Nlink` equal to 2 plus its live subdirectories, `TrashInodes` and
`TrashBytes` equal to a recount; placement distinctness. It also counts the
orphan shards on that node (files the chunk table does not place there, plus anything in
the shard tree that is not a shard) and lists the first 1,000 by key (`orphans` in the JSON
reply), so a caller can look up each one's chunk history; orphans are informational, the
sweep removes them after grace. Data is verified by `check --scrub` (§5), not by fsck, and there is no
`--fix`: the FSM keeps refcounts exact (I-27), so any finding is a bug to report, not
state to patch.

## Invariants

- **I-13** — No shard referenced by committed metadata is ever deleted: GC deletes only chunks with `Refs == 0` older than grace, repair deletes old shards only after the new placement is committed, and Sweep deletes only shards the replicated chunk table does not place on this node.
- **I-14** — A shard file (or S3 object) younger than the grace period is never deleted by Sweep, the S3 sweep, or a GC/repair DELETE, regardless of what the chunk table says.
- **I-15** — Repair restores k+m slots for any chunk with ≥ k good live shards, and migration to a new (k,m) never passes through a state in which fewer than k shards decodable under the committed placement exist.
- **I-41** — A tombstoned chunk re-referenced before GC is revived without re-upload, and its `Refs` is exact.
- **I-42** — Draining a node moves every one of its slots before `node remove` succeeds without `--force`; after removal no committed placement references the node except via `--force`-acknowledged degraded slots.
- **I-43** — `GCDelete` applied on any replica deletes exactly the same chunk entries (determinism via `Before` in the command).
- **I-44** — `fsck` reports zero findings on a cluster where every test in this document passed; any finding is a bug, not noise.
- **I-65** — A repair pass never lowers the number of readable slots of a chunk and never proposes a placement equal to the committed one: a slot whose shard is still readable (a live draining node) stays in the placement until a replacement is placed, a live draining holder counts toward the `≥ k` readability test, and a pass that could improve nothing proposes nothing.
- **I-174** — A repair pass works on at most `Workers` chunks at once (`repair.workers`,
  1..64, default 4); each chunk's move sequence is unchanged (I-65, I-102); the pass
  statistics do not depend on `Workers`; `Progress` reports the pass while it runs and
  nothing once it ends; a cancelled pass starts no more chunks and returns once the ones
  in flight end.
- **I-175** — A shard request marked background holds at most half of a node's PUT and,
  separately, GET slots (never fewer than one); a client-path request never waits behind
  a background one for the other half; a background request is queued, refused only as busy once it has waited `SlotWait` (I-246),
  and the mark changes no reply; every request a repair pass makes carries the mark and
  no gateway read or flush does.
- **I-157** — A delete reclaims nothing: only `Purge`, `Unlink` of an ephemeral name
  and `Truncate` drop references, `meta.IsEphemeral` is the one list of names that are
  unlinked rather than trashed (an upload temp, a thumbnail, `._<name>`, `.DS_Store`),
  and the leader's retention purge takes exactly the roots whose `DeletedAt` is older
  than `trash.retention` on the FSM clock (`max(leader clock, LastNow)`), in bounded
  batches, never when the retention is zero.
- **I-215** — The background scrub verifies every chunk with `Refs > 0` at least once in
  any span of `repair.scrub_period` (P) covered by sweeps, and a sweep scrubs only the
  chunks whose phase `BigEndian(hash[0:8]) mod P` lies in the arc from the previous
  sweep's start (or `SweepInterval` before a leader's first sweep) to its own start, every
  chunk when that arc is P or longer, none when P is 0, and a pass that is not a sweep
  scrubs none; `check --scrub` verifies every chunk whatever P.
- **I-377** — The FSM keeps the live chunks grouped by placement class, `(k, m, small,
  sorted slots)` with each class's chunk count, shard bytes and members, in step with
  every applied command and rebuilt on `Restore`; it equals a recount of the state after
  every command, and is never encoded.
- **I-378** — `status`, `check` and the leader's stats sample derive the degraded,
  repair-pending, unreadable, policy-stale and host-skew counts and every node's slots,
  shard bytes and missing slots from the placement classes, each class judged once by a
  representative that derives the same state as every member; no chunk is visited, except
  the members of classes in those states for `check --files`; the disk skew is the
  leader's last sweep's.
- **I-379** — A repair pass that is not a sweep visits only the members of classes with a
  bad slot or a pending migration that are not stuck, the chunks with a reported slot,
  and at most `HomeBatch` chunks of the home queue (while the gate is open) and
  `SumsBatch` of the sums queue; a sweep visits every live chunk and runs on the first
  pass as leader, after `SweepInterval`, when the home candidate set changed, for
  `check --scrub`, while the gate is open with an empty queue, on the first pass with
  no class needing work after a pass re-placed chunks, and on a pass where a target can
  take shards that could not at the last sweep, when that sweep left away bytes
  unmovable.
- **I-380** — A class whose chunks a pass visited without committing or attempting a new
  placement for any of them is skipped by the passes after it until its view (each
  member's state, liveness and fitness to take a shard, and the volume's policy) changes,
  `StuckRetry` passes or a sweep visits it; a class with a failed attempt is visited
  again by the next pass.
- **I-195** — Every path under `/.tailraid/` is ephemeral: a replace or a delete of one
  unlinks it at once and never trashes it, and the web file browser lists nothing under it
  (I-160); on the mounts the tree is plain files and folders.
- **I-182** — With `mirrors.every > 0` a mirror's host begins an automatic snapshot,
  named by the UTC minute, on each of its online mirrors with no sync running whose
  newest automatic snapshot is older than `every`, so the first tick after an offline
  stretch catches up at once; it begins nothing for an offline mirror, so nothing is
  held for it; it keeps on each mirror the newest automatic snapshot of each of the last
  `hourly`, `daily`, `weekly` and `monthly` buckets and deletes the other automatic ones
  oldest first, and the oldest kept automatic one too while the mirror is under its
  free-space reserve, never a manual one; it ends a sync of its own whose clone it lost;
  the leader ends as failed any sync older than `hold_max`; an `ErrExist` on the
  automatic name is not retried; and nothing is proposed while a live member lacks
  codec 7.

## Tests

| Invariant | Tests |
|---|---|
| I-13 | `TestChunkRPC_DeleteFence`, `TestCluster_GCDoesNotDeleteInflightUpload`, `TestGC_CollectsAfterGrace_RespectsFence_AndSweep`, `TestModelCheck_Long`, `TestReset_WipesRaftKeepsShards`, `TestSweep_OldPlacedHereKept`, `TestSweep_SkippedWhenNotHealthy` |
| I-14 | `TestChunkRPC_DeleteFence`, `TestCluster_GCDoesNotDeleteInflightUpload`, `TestFlusher_LateRetry_RePutsBeforeCommit`, `TestGC_CollectsAfterGrace_RespectsFence_AndSweep`, `TestGC_LeaderClockBehind_DoesNotStallGC`, `TestStore_DeleteIfOlder_RefusesYoungShard`, `TestSweep_YoungShardKept` |
| I-15 | `TestModelCheck_Long`, `TestRepair_DeadNodeHoldDown_ThenReplaced`, `TestRepair_FillsDegradedSlotsWhenSpareAppears`, `TestRepair_MigrationCrash_Resumes`, `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK`, `TestRepair_SmallChunk_SetRedundancy_Migrates` |
| I-41 | `TestFSM_Revive` |
| I-42 | `TestCluster_JoinDrainRemove`, `TestRepair_DrainMovesAllSlotsBeforeRemove` |
| I-43 | `TestFSM_DeterministicAcrossReplicas` |
| I-44 | `TestCheck_MPlusOneCorrupt_NotOK`, `TestFsck_CleanClusterNoFindings`, `TestFsck_CountsOrphanShards` |
| I-65 | `TestRepair_DeadWithinFillAfter_NoRepeatProposal`, `TestRepair_DrainNoReplacement_KeepsShard`, `TestRepair_DrainingHolderIsReadable`, `TestRepair_Migration_WithinCap_VoterHostDisks` |
| I-174 | `TestRepair_FanOut_Bounded`, `TestRepair_FanOut_CancelDrains`, `TestConfig_RepairWorkersAndRate`, `TestPhaseFanOut_ConformingPassPasses`, `TestPhaseFanOut_OverWorkersFails`, `TestPhaseFanOut_FastPassSkips` |
| I-175 | `TestServer_BackgroundShare`, `TestBackgroundSlots_Table`, `TestClient_BackgroundHeader`, `TestPhaseFanOut_ConformingPassPasses`, `TestPins_WalkFetchesBackground` |
| I-157 | `TestVFS_Remove_EphemeralIsImmediate`, `TestGC_PurgeTrash_RetentionBoundary`, `TestGC_PurgeTrash_DrainsInBatches`, `TestConfig_TrashRetention`, `TestSoak_AllPhasesAgainstHarness` |
| I-215 | `TestScrubDue_Window`, `TestRepair_ScrubPeriod_CoversEveryChunkOnce`, `TestRepair_ScrubPeriod_FirstPassAsLeader`, `TestConfig_ScrubPeriod` |
| I-377 | `TestFSM_Stats_IncrementalMatchesRecompute`, `TestClasses_GroupBySlotSet`, `TestModelCheck_Long` |
| I-378 | `TestClasses_RepresentativeMatchesEveryMember`, `TestScanHealth_FromClassesMatchesEveryChunk` |
| I-379 | `TestRepair_HomeReturns_SweepsAndMovesHome`, `TestRepair_PassVisitsOnlyWhereWorkIs`, `TestRepair_ScrubPeriod_CoversEveryChunkOnce`, `TestRepair_RefillGoesHome_Table` |
| I-380 | `TestRepair_StuckClassWaitsForTheView`, `TestRepair_PutFails_Retries` |
| I-195 | `TestWebUI_Ls_HidesTailraidTree`; `TestStub_I_195` (planned with the trash of docs/02: TestVFS_TailraidTree_IsEphemeral) |
| I-182 | `TestSoak_AllPhasesAgainstHarness` (phase 19), `TestSchedule_TakesAndThins_Table`, `TestSchedule_OfflineMirrorNothingHeld_CatchUpOnAttach`, `TestSchedule_ThinThenSweep`, `TestSchedule_LostAndExpiredSyncsEnded`, `TestConfig_Mirrors`, `TestSchedule_AutoNameTaken_NoRetry`, `TestSchedule_SkipsWhileCodecOld` |
