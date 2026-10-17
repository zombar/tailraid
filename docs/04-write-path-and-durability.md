# 04 — Write path and durability

This document fixes how bytes travel from an NFS or S3 client into the cluster, when they
are acknowledged, and what that acknowledgement means. It is the normative source for the
durability contract quoted in the README.

## 1. What the NFS server actually calls

`willscott/go-nfs` v0.0.4 translates each NFS v3 RPC into `billy.Filesystem` calls. The
sequences below are verified against the vendored source and are the contract the `vfs`
package must satisfy.

| RPC | billy calls (in order) | Reply |
|---|---|---|
| WRITE(fh, off, data) | `Stat(path)`, `OpenFile(path, O_RDWR)`, `Seek(off, io.SeekStart)`, `Write(data)`, `Close()` | always `FILE_SYNC` |
| COMMIT | none | `OK` (no-op) |
| READ(fh, off, n) | `Open(path)`, `ReadAt(p, off)`, `Close()` | data or short read + eof |
| SETATTR size | `OpenFile(path, O_WRONLY\|O_EXCL)`, `Truncate(size)`, `Close()` | — |
| SETATTR mode/uid/gid/times | `Change(fs).Chmod/Lchown/Chtimes` | — |
| CREATE | `Create(path)`, `Close()` | — |

Consequences:

- There is no open-to-close session. Every WRITE opens and closes the file. Buffering "until
  close" is wrong; the previous project did exactly that and corrupted every multi-RPC file.
- Because the reply is `FILE_SYNC` and COMMIT is a no-op, **`Close()` is the durability
  point** from the client's perspective. Whatever `Close()` returns success for must survive
  a crash.
- WRITE sizes are bounded by the client's `wsize` (64 KiB default on macOS, 1 MiB max on
  Linux). Offsets are arbitrary and arrive out of order under parallel RPCs.

## 2. Overlay: dirty extents + write-ahead log

Each gateway keeps, per inode with unflushed data, a `dirtyFile`:

```go
type extent struct { off, n uint64; walOff int64 }  // dirty [off, off+n) lives at walOff in the WAL
type extents []extent                                // sorted by off, non-overlapping

type dirtyFile struct {
    mu       sync.Mutex
    ino      meta.Ino
    size     uint64   // overlay size, authoritative for GETATTR while dirty
    minSize  uint64   // the truncation point: lowest truncate since the last flush (valid only with sizeSet)
    sizeSet  bool     // a truncate happened since the last flush
    ext      extents
    wal      *WAL
    dirtyB   uint64
    lastW    time.Time
    flushing bool     // one flush per inode at a time; a new plan waits for the in-flight one
}
```

`minSize` is what makes truncate-then-grow correct: after `truncate(1.5 MiB)` followed by
`write @5 MiB`, `size` is back to 5 MiB but `minSize` is 1.5 MiB, and every base byte at
or beyond `minSize` that no extent covers must read (and be committed) as zero.

Extent-set semantics (all O(log n + k) on the sorted slice):

- `insert(x)`: remove the parts of existing extents covered by `[x.off, x.off+x.n)`,
  splitting an extent into two when `x` lands inside it, then insert `x`. Later writes win
  byte-for-byte (last-writer-wins at byte granularity within one gateway).
- `overlapping(off, n)`: extents intersecting the range, in offset order.
- `truncate(size)`: drop extents at or beyond `size`, clip the one straddling it; set
  `size`, `minSize = min(minSize, size)`, `sizeSet = true`. A truncate *up* only sets
  `size`; the gap reads as zeros.
- `Write` beyond the current size extends `size`; the hole between old size and `off` reads
  as zeros (no extent is created for it).
- `Read(off, p)`: WAL extents first; base bytes only for offsets `< minSize` (or all base
  bytes when `!sizeSet`); everything else zero. This is the read-your-writes view (I-34).
- `minSize` is set by the first truncate since the last flush and lowered by later ones;
  without a truncate the overlay has no size of its own: the client-visible size is
  `max(committed size, end of the dirty extents)` and base bytes are valid up to the
  committed size *as of the read or flush*, never as of the entry's creation. A dirty
  entry therefore follows other gateways' Commits (they may grow or shrink the file
  between this gateway's touches, L-178) and an entry with nothing left to flush is
  dropped after its Commit, so the next touch starts from the current committed size.
  Every byte assembled for a flush comes from the plan's extents or from the base chunk
  as it is at flush time; a rewritten chunk is always `min(ChunkSize, size − idx·ChunkSize)`
  long for the size the Commit will carry.

### 2.1 WAL record layout

An inode's log is a run of **segment** files under `<data_dir>/staging/`: segment 0 is
`<ino>.wal` (the single file of releases before segments, read as segment 0), segment n is
`<ino>.<n>.wal`. Records are appended to the newest segment, the *head*:

```
record := crc32c u32 | type u8 | off u64 | len u32 | data[len]
type    := 1 write    off = file offset, data = the bytes
           2 truncate off = new size, len = 0
           3 base     off = committed size the following records apply to, len = 0
           4 actor    off = the user whose writes follow (docs/21 §4), len = 0
           5 state    off = overlay size; data = minSize u64 | sizeSet u8: the inode's size
                      and truncate state when this segment began (its first record)
           6 clean    off = W, the log position when the committed plan was captured;
                      data = size u64 | minSize u64 | sizeSet u8 | ranges u32 | (start u64,
                      end u64)…: the file ranges a Commit made durable and the inode's state
                      after it
           7 attrs    off = mask (mode 1, uid 2, gid 4, atime 8, mtime 16); data = mode u32 |
                      uid u32 | gid u32 | atime i64 | mtime i64: a client's SETATTR on the
                      dirty file, pending until its flush (§6, I-401)
```

`crc32c` covers `type|off|len|data`. All integers little-endian. A *log position* is
`n·2⁴⁰ + offset` in segment n, so positions grow across segments and an extent's `walOff`
names its segment.

```go
type WAL struct { fs sysfs.FS; dir string; ino meta.Ino; head sysfs.File; segs []segment }
func openWAL(fs sysfs.FS, dir string, ino meta.Ino) (*WAL, error)            // a new segment's directory entry is fsynced before it is used
func (w *WAL) AppendWrite(off uint64, p []byte) (walOff int64, err error)   // write + f.Sync()
func (w *WAL) AppendTruncate(size uint64) error                             // + f.Sync()
func (w *WAL) AppendClean(c cleanRec) error                                 // + f.Sync()
func (w *WAL) ReadAt(p []byte, walOff int64) error                          // from whichever segment holds walOff
func (w *WAL) Replay(fn func(typ uint8, off uint64, data []byte, walOff int64) error) error
func (w *WAL) DropBefore(pos int64) error                                   // remove every segment wholly below pos, oldest first
func (w *WAL) Compact(recs []walRec) ([]int64, error)                       // write recs to a fresh segment, fsync, rename, drop the rest
```

Rules:

- `AppendWrite`, `AppendTruncate` and `AppendClean` return only after `f.Sync()` succeeds.
  There is no "no-sync" mode: the struct has no such field, and tests that want speed put
  the staging directory on a tmpfs or accept the cost. All file access goes through the
  `sysfs.FS` seam so tests can inject a failed fsync or a crash at any step.
- A segment file is created on the first write to an inode and whenever the head reaches
  `SegmentSize` (64 MiB). Its directory is fsynced before the first record is appended,
  so an acknowledged record never depends on a directory entry that power loss could drop
  (I-63). A new head begins with a `state` record and, when a user wrote last, an `actor`
  record; the head is rotated only once every record in it is fsynced, so a group-commit
  fsync covers one file. After a failed fsync the un-synced records are cut back off the
  head so a later crash cannot replay bytes that were never acknowledged (I-37).
- `Replay` reads the segments in order and each **stops at the first record whose CRC
  fails or whose length runs past EOF** (the torn tail from a crash mid-append); a torn
  record ends the log, and any later segment is removed. Everything before it is replayed
  into the extent set: a `base` resets the inode, a `state` initialises it only when no
  earlier record did, and a `clean` drops every extent piece whose `walOff` is below its W
  inside its ranges and sets the size and truncate state it carries.
- **A committed flush appends, it does not copy** (I-397). `CommitFlushed` appends one
  `clean` record naming the chunk ranges the Commit made durable, fsyncs it, then removes
  every segment that lies wholly below the lowest live extent's position, oldest first, so
  a crash leaves a suffix of the run that replays to the same state. A file written front to
  back is flushed front to back, so its segments go in order and its log holds about the
  dirty bytes plus one segment, whatever the file's size. When nothing is left dirty the
  inode's segments are removed (`Reset`).
- **Compaction** is for a region written over and over, where the extents stay few while the
  log grows. It runs in `CommitFlushed` only when the dead bytes reach the live bytes plus
  `CompactMin` (128 MiB) *and* the live bytes are at most `CompactMax` (128 MiB): the live
  records are streamed through one bounded buffer (1 MiB) into a fresh segment, fsynced and
  renamed into place, and the older segments are then removed oldest first. A crash after
  the rename replays the old segments and then the new one, whose records rewrite the same
  bytes, so either order of failure replays the same state; a stale `<ino>.<n>.wal.new` is
  removed at open. A failure after the rename leaves the inode on the new segment with
  correct record offsets, never on a closed handle (I-80). A WRITE therefore never waits on
  a copy of more than `CompactMax` bytes; a log with more live bytes is never copied.
- The flusher flushes an inode in full, regardless of the idle timer, when its log's dead
  bytes reach its live bytes plus `CompactMin` (L-186), so a rewritten region's log stays
  bounded, and a WRITE waits while the inode's log is at its cap (§3).
- `OpenOverlay` replays every inode's segments at startup, so a gateway that crashed with
  dirty data comes back with the same overlay and flushes it.

**Group commit.** `AppendWrite` is two steps: the record is written at the end of the log
under the inode lock, then the caller waits, without the lock, for an fsync that started
after its bytes were written. The first waiter runs the fsync; every record written before
it started is covered by it and returns; a record written while it runs waits for the
next one. One fsync therefore covers however many WRITEs reached the inode while the
previous fsync ran (the NFS gateway serves a connection's requests concurrently, docs/07),
and each WRITE is still acknowledged only after its own record is on disk (I-7). The
overlay applies a record's in-memory effect (the extent, a truncate) only once its fsync
has succeeded, in log order, so a READ never shows bytes whose WRITE may still fail and the
live view always equals what a replay of the log would build. A failed fsync fails every
append it covered: the log is cut back to the last fsynced byte and the file reopened
(I-37, I-101) before any later append, and the cut records' effects are dropped unapplied.
A flush snapshot, a compaction and an eviction wait for the inode's appends between write
and fsync, and hold new ones until they are done (I-168).

Per-WRITE cost: one `pwrite`, one `fsync` shared with every WRITE that arrived during the
previous one, one sorted insert. Nothing is held in memory but extents.

## 3. Flush triggers

| Trigger | Default | Action |
|---|---|---|
| inode idle (no WRITE for) | 2 s | flush that inode |
| inode dirty bytes ≥ `FileDirtyHi` | 32 MiB | partial flush: its fully covered chunks now; the partial tail chunk stays dirty |
| inode log's dead bytes ≥ its live bytes + `CompactMin` | 128 MiB | full flush of that inode (its log compacts, §2.1, L-186) |
| inode log ≥ `WALCap` | the stage, at least 128 MiB | a WRITE that arrives while the inode's log is at the cap **waits** (next row) |
| total dirty bytes ≥ `TotalDirtyHi` | 256 MiB | flush the largest inodes until below |
| total dirty bytes ≥ `MaxDirty`, **the stage** (fresh **and** in-flight plan bytes) | `staging.size`, else a quarter of the free bytes of the staging directory's filesystem, at least 1 GiB (I-396) | a WRITE **waits** (next row) while the overlay holds `MaxDirty` or more dirty bytes; existing dirty data is intact and drains through the triggers above |
| the staging filesystem's free bytes < `StageReserve` | 2 % of its size, at least 1 GiB | a WRITE **waits** (next row) until flushes free room; the free bytes are read at most once a second (I-396) |
| a WRITE meets the WAL cap or `MaxDirty` | `CapWait` 20 s | the WRITE blocks until the flusher frees room (a Commit applied, an inode evicted) and then proceeds; it fails only when `CapWait` expires (`NFS3ERR_IO`, `EIO`) or, at once with `ENOSPC`, when that inode's last flush attempt failed because the cluster is full (L-189); any other flush failure (a leader or peer restarting, a codec refusal) is waited out like a slow flush (L-219), so a client that outruns replication is throttled to it rather than told the disk is broken (I-79). The waiting WRITE holds one of its connection's `InFlight` request slots (docs/07, I-167); `CapWait` is shorter than the clients' soft-mount timeouts (docs/07) |
| shutdown | 30 s (`ShutdownFlush`) | `FlushAll` with that deadline, then exit; whatever did not flush stays in the WAL and flushes on the next start |

**The stage.** `MaxDirty` is how much a gateway accepts ahead of the cluster: a copy runs
at the speed of the local disk until the stage is full, then at the speed the flusher drains
it. It is `staging.size` when the config sets it, otherwise a quarter of the free bytes of
the filesystem holding `<data_dir>/staging` when the overlay opens (the bytes its logs
already hold counted as free), never under 1 GiB; the per-inode cap `WALCap` is the stage,
never under 128 MiB, so one large file may use the whole stage (I-396). Staged bytes are
acknowledged and durable on this host's disk only: until the flusher commits them, losing
that disk loses them, as for any acknowledged write (§8). `status` shows them as
`gateway.staged` with the stage and the drain rate, the dashboard as a "staged" row with
the time to drain, and `check` warns while more than 1 GiB is staged (docs/12 §8, I-398).
`update --restart` waits for the stage to drain (I-221), so a large stage lengthens that
wait; `--now` skips it.

Flush workers run per inode from a bounded pool (default 4) so a big copy does not starve
small files; across all of them at most `ChunkWorkers` (default 8) chunks are in memory at
once (§9).

## 4. `FlushInode` — the exact procedure

```go
func (f *Flusher) FlushInode(ctx context.Context, ino meta.Ino) error
```

1. **Snapshot the plan.** `plan := ov.snapshotForFlush(ino)` atomically takes the current
   extents, `size`, `minSize`, `sizeSet` and installs a fresh empty extent set (whose
   `minSize` starts at `plan.size`). New WRITEs continue to append to the same WAL and land
   in the fresh set. The WAL is not reset here. At most one flush per inode is in flight
   (`flushing`); a trigger that fires while one is running marks the inode for a follow-up
   flush rather than starting a second plan, so plans are applied in order.
2. **Base inode.** `base, ok := st.GetInode(ino)`. If `!ok` the file was unlinked (or a
   rename replaced it) → `ov.Drop(ino)`, return nil. Nothing is committed for a dead inode.
3. **Touched chunk set.** `CS := Vol.ChunkSize`; `n = ceil(plan.size/CS)`. Touched =
   chunks intersecting any dirty extent ∪ (when `plan.sizeSet`) `{ceil(plan.minSize/CS) − 1}`
   if `plan.minSize % CS != 0` (the chunk straddling the truncation point must be rewritten
   because the FSM cannot trim bytes; it rejects such a Commit with `ErrNeedTailRewrite`)
   ∪ every index `i` in `[ceil(plan.minSize/CS), n)` that no extent covers and that is not
   already `HashZero` in `base` (these become `HashZero`: the bytes there were truncated
   away and must not resurface). For a partial flush (`FileDirtyHi` trigger) touched is
   restricted to chunks whose range `[i·CS, (i+1)·CS)` is fully covered by extents, and
   `plan.sizeSet` plans are never partial.
4. **Assemble each touched chunk.** Effective length `eff = min(CS, plan.size − i·CS)`. If
   dirty extents fully cover `[i·CS, i·CS+eff)` read them from the WAL; otherwise read the
   base chunk (`Reader.ReadChunk(base.Chunks[i])`, zeros when `HashZero` or `i ≥
   len(base.Chunks)`), **zero every base byte at offset ≥ `plan.minSize`** when
   `plan.sizeSet`, and overlay the WAL extents. Clip to `eff`.
5. **Holes.** If the assembled chunk is all zero → `ChunkWrite{i, HashZero}`; no upload.
   An index past the base list that no write touches is not in the touched set at all:
   the FSM grows the list with `HashZero`, so a truncate followed by a far seek costs
   one `ChunkWrite` for the written chunk, not one per index in between (I-80).
6. **Hash and dedup.** `h = sha256(chunk)`. If `st.GetChunk(h)` exists (live or tombstoned)
   and **at least `k` of its placement slots are present** (on a live node that answers
   `HEAD` for the shard; liveness alone is not trusted, a live node may have lost the
   file) → `ChunkWrite{i, h}` with no upload; the Commit revives a tombstone. If the entry
   exists but has fewer than `k` present slots (degraded beyond readability, a tombstone
   whose holders died — tombstones are never repaired — or holders that lost their shards),
   the flusher has the bytes in hand: it re-encodes with the entry's recorded `(k, m)`,
   PUTs every slot that is not present to fresh nodes (re-place excluding current holders),
   and proposes `SetPlacement` for the filled slots before the Commit. An acknowledged
   write is never committed against an unreadable chunk.
7. **Policy.** `(k, m) = len(chunk) ≤ SmallChunk ? (1, Vol.M) : (Vol.K, Vol.M)`.
8. **Place.** `placement = Place(h, liveCandidates(peers.Live(), nodes), k+m)` where
   candidates exclude `Draining`/`Removed` nodes and nodes that answered `ErrFull`, built
   from this gateway's own prober view.
9. **Encode and PUT.** `shards = codec.Encode(chunk)`; PUT every slot in parallel (self via
   `local.Put`) under key `hex.k-m.idx`. Wait until **≥ k acks AND (all PUTs finished OR
   10 s elapsed)**. Slots that failed are re-placed once (excluding failed nodes and nodes
   already holding an acked slot of this stripe; slot `i` keeps index `i`) and PUT again;
   the original PUTs of re-placed slots are **cancelled** and the chunk waits for every PUT
   goroutine to return before it is decided, so no straggler keeps a shard buffer alive
   past its chunk (a late shard that landed anyway is an orphan swept after grace, L-190,
   I-83). Once k acks are in, the wait for the rest is capped by a **straggler grace** of
   `clamp(4 × p95 of recent PUT durations, StragglerMin 2 s, PutDeadline)` measured from
   the k-th ack: a PUT still pending at the grace is treated exactly as one pending at the
   deadline, and its host is *held slow* (docs/05 §6) so the chunks that follow place
   around it for the hold instead of waiting on it again (I-169, I-371, L-209). The floor keeps a 256 KiB/s
   DERP peer (1.4 s for a 350 KiB shard) in the stripe; the multiplier follows a slow
   cluster. Slots still missing are recorded as `0` (degraded). If acks < k → return an
   error; the plan stays dirty and is retried with backoff (1 s doubling to 60 s); when
   every failed slot was `ErrFull` the error wraps `chunk.ErrFull` so the client sees
   `ENOSPC` rather than `EIO` (I-79). An ack means the receiver fsynced file and directory
   (I-7). Steps 4–9 run per chunk as one pipeline under a flusher-wide semaphore of
   `ChunkWorkers`: the chunk's bytes and shards are released the moment it is decided, and
   nothing of it but `(idx, hash, k, m, placement)` survives into step 10 (I-78). The flush
   does not populate the read cache; the first read after a flush fetches shards and the
   `Reader` caches then, so a bulk upload never evicts the hot set. The flusher also
   hashes the k data shards it just encoded (`Sums`, 32 B each): they are a pure function
   of the chunk's bytes and `(k, m)`, so every node that encodes the chunk computes the
   same ones, and the read path uses them to serve a range from one shard before the whole
   chunk is in hand (§5, I-201, I-202). The `Commit` carries them only while every live
   member reads codec 5 (02 §Codec versions); otherwise the chunk is committed without them
   and the repair scanner's backfill records them (06 §4).
10. **Commit.** A plan is committed in **batches** of at most `FileDirtyHi` bytes of chunks
    (8 at 4 MiB): `assemble` keeps the lowest-indexed touched chunks that fit and leaves
    the rest dirty as residual, exactly like a partial flush; a plan with a truncate is
    never split (I-80, L-191). Then `Propose(Commit{Ino, BaseVersion: base.Version,
    HasTruncate: plan.sizeSet, TruncateTo: plan.minSize, NewSize, Writes, NewChunks, Now,
    Rewrite, RewriteStart})` (the last two while every live member reads codec 9, §6:
    `RewriteStart` when `plan.sizeSet` and `plan.minSize` is 0, else `Rewrite` is the
    rewrite id the gateway holds for the inode, or 0) where `NewSize` is:
    - full flush, no truncate: `max(base.Size, maxExtentEnd)`;
    - full flush with truncate: `max(plan.minSize, maxExtentEnd)`;
    - partial or batched flush: `max(base.Size, min(end of the last committed chunk,
      plan.size))`; the dirty tail beyond it stays in the overlay and is committed by the
      next flush, so the committed size is never ahead of committed data.
    Error handling:
    - `ErrConflict`: another gateway committed first. Reload `base`, recompute steps 3–9 for
      the same plan (dedup makes untouched chunks free), propose again with `NewSize`
      recomputed from the *new* base by the rules above: another gateway's append is
      preserved, and our truncate still wins over it (§6 states the resulting rule for
      concurrent writers). Dirty extents are never dropped on
      conflict (I-35).
    - `ErrUnknownChunk`: a dedup'd chunk was GC'd between step 6 and commit. Upload it (steps
      7–9 for that chunk) and propose again.
    - Any retry of the Commit (conflict, unknown chunk, `ErrLeadershipLost`, timeout)
      re-runs steps 4–9 for every chunk of the plan: the bytes are **re-assembled from the
      plan's WAL extents** (the WAL is untouched until step 11) over the base as it is now,
      and since a chunk is registered only by the Commit that applies, the dedup lookup
      misses and the PUTs are repeated. PUTs are idempotent and refresh the shards' mtime,
      which guarantees the orphan-shard sweep (06) cannot have removed them in the meantime
      however late the retry. Holding the assembled bytes across the retry would cost a
      plan's worth of memory per inode for nothing (I-78).
    - `ErrNotExist`: inode gone → `ov.Drop(ino)`, return nil.
    - `ErrSuperseded` (codec 9, §6): another rewrite of the file started after this
      gateway's → `ov.Drop(ino)`, forget the rewrite id, count it in `status`
      (`gateway.rewrites_superseded`), log once per inode at Info, return nil. The chunks
      uploaded for the refused Commit are orphan shards (L-190).
    - `ErrNeedTailRewrite`: programming error in step 3; log at error level and retry with
      the tail chunk included.
11. **Wait and clear.** `st.WaitApplied(index)`; `ov.commitFlushed(ino, plan)` removes the
    plan's extents from the file's bookkeeping; if no extents remain, the inode leaves the
    overlay and its WAL file is removed (`evict`); otherwise the WAL is compacted by
    streaming (§2). Either way the flusher reports the attempt's outcome to the overlay
    (`NoteFlush`), which is what a waiting WRITE consults (§3), and wakes every WRITE
    waiting at a cap.

Step 9's "≥ k acks" is the user's chosen degraded-write policy (see 05 §5). Full
redundancy is restored by the repairer (06).

## 5. Read path

```go
func (r *Reader) ReadChunk(ctx context.Context, h meta.Hash) ([]byte, error)
func (r *Reader) ReadAt(ctx context.Context, ino meta.Inode, p []byte, off int64) (int, error)
```

1. `HashZero` → caller zero-fills. Cache hit → return: the hottest 64 MiB of chunks sit
   in memory (verified when inserted, a subset of the disk entries); a disk hit verifies
   sha256 and promotes the chunk to memory. A hit refreshes the file's mtime at most once
   a minute (the on-disk order only matters across restarts). Concurrent reads of one
   chunk share a single fetch.
2. `c := st.GetChunk(h)`; missing → `EIO` (metadata says the chunk exists; this is a bug or
   a lost majority, not a normal path).
3. `order = peers.Rank(live(nonZero(c.Placement)))`: self first, then by RTT, S3 nodes last; holders the prober reports dead are never dialled (L-131). The ranked order is then stably partitioned: this host's own shards first (no network), then data slots, then parity, whatever the RTT, so a decode is needed only when a data holder is dead or hedged, or this host holds a parity shard of its own (L-63).
4. Fetch the first `k` shards in parallel. On any failure or `ErrCorrupt`, pull the next slot
   in `order` until `k` good shards or the list is exhausted (→ `EIO`, logged with the chunk
   hash and the slots tried).
5. `codec.Decode(shards, c.Size)`; verify `sha256 == h`. Mismatch → `EIO` for every reader
   still waiting on the chunk, the event E-134 and an error log; the scrubber finds the
   bad shard (06 §5).
6. `cache.Put(h, chunk)`.

**A range from one data shard (I-202).** The web UI, NFS and SMB ask for ranges, and a
client that opens a video or seeks in it waits for the first bytes of one chunk; waiting
for the whole 4 MiB (k shards, a decode and a hash) before the first byte is what the
user felt as slow (measured 2026-10-11, docs/reports/bench-2026-10-open-media.txt). With
systematic Reed-Solomon, data shard *j* holds the chunk's bytes `[j·S, (j+1)·S)` with
`S = ShardLen(c.Size, k)` (03 §Shards), so a range inside one data shard needs only that
shard. `vfs.File.ReadAt` therefore reads through `Reader.ReadAtEarly`: the fetch is the
same (step 4, the data slots first), but as each data shard arrives it is hashed and
compared with `c.Sums[j]`, the hash recorded with the chunk in Raft; a shard that matches
is published to the readers waiting for a range inside it, who copy their bytes and
return while the other shards are still in flight; a shard that does not match is a bad
slot (`BadSlot`, as a corrupt frame) and the fetch pulls the next slot. A range that
crosses data shards is served once each of them verified (their bytes are exactly what a
decode would concatenate, each checked against Raft); a range reaching a shard that never
verified (its holder dead, its GET failed, its sum wrong) and a chunk without `Sums` wait
for the decode as before, and every byte at or past `c.Size` is zero-filled by `ReadAt`,
never taken from shard padding.
Steps 5 and 6 still run: the chunk enters the cache only once it hashed to `h`. Because
early bytes are verified against committed metadata, nothing a client receives can differ
from the chunk's content (I-40). The flusher's base reads (`BaseReader`, §4 step 3),
repair, scrub and the pins worker keep the strict `ReadAt`/`ReadChunk`. `status` counts
`reads_early` (12 §8).

`ReadAt` walks chunk indexes, copies `min(eff, c.Size)` bytes from each chunk and zero-fills
the remainder (a short dedup'd chunk under a truncate-up). The overlay is consulted first:
`Overlay.Read` merges WAL extents over base bytes.

**Readahead.** A chunk fetch costs a network round trip plus the transfer, and the NFS
and SMB clients ask for one range at a time, so without readahead a sequential read moves
one chunk per fetch latency whatever the link. `vfs.File.ReadAt` (the gateways and the
web UI; never the flusher's base reads) therefore calls `Reader.Prefetch(inode, off)`
before it reads: a READ of chunk *i* is *sequential* when *i* = 0 or *i* is the chunk
after the inode's previous READ, and a sequential READ starts a background fetch of chunks
*i+1 … i+depth* into the cache, where the depth is `ReadaheadDepth(ChunkSize)` =
`min(ReadAhead, ReadAheadBytes / ChunkSize)`, at least one: readahead reaches
`ReadAheadBytes` (8 MiB) ahead of the client, two chunks at 4 MiB, four at 1 MiB (I-203).
A prefetch is never started for `HashZero`, for a
chunk the cache already holds, for one already in flight, by a non-sequential READ, or by
a reader without a cache; at most `ReadAhead` (4) prefetches run gateway-wide, and a further
candidate is skipped, never queued. A prefetch registers in the in-flight table before it
starts, so a client READ that reaches a chunk being prefetched joins that fetch instead of
starting another; its result goes only to the cache, and its failure is logged at debug
and changes nothing the client sees (the client's own READ of that chunk fetches it
normally). `Reader.Prefetching()` reports the prefetches in flight; `status` carries the
chunks readahead fetched and the client READs it served (docs/12 §8).

Three rules keep readahead out of the client's way (I-203; measured on the real cluster
on 2026-10-11, the head of a file took 0.7 s against 0.25 s for a chunk fetched alone,
because the four prefetches started alongside the chunk the client was waiting for).
`vfs.File.ReadAt` starts the prefetch only once its own read is served, so the chunk the
client waits for never competes with the ones it may want later. A prefetch's shard GETs
run under the background mark (`chunk.Background`, 06 §4, I-175), so a holder serves a
client's own GET first; a client READ that joins a prefetch in flight keeps that call and
is documented to queue behind foreground traffic for that one chunk. If that prefetch fails,
the READ fetches the chunk itself at foreground priority instead of returning the
prefetch's error: a holder may refuse or time out a background GET under load, which says
nothing about the client's read (found by CI: phase 12 read EIO through another gateway).

Readahead follows two **streams** per inode, because a player reads a video from its
start while it asks for the file's tail (an MP4's index often sits there): a READ that
continues either stream stays in the sequence, and with one stream these two requests
cancelled each other's prefetches at every switch, so readahead never ran. A stream's
**frontier** is the furthest chunk a READ of it reached, and a READ continues it from
`ReadAhead` chunks before the frontier to `ReadAhead` after, because a client's
concurrent READs are served out of order: an NFS client keeps up to `InFlight` READs in
flight (07), and through the macOS mount a READ of the chunk after the frontier landed
before the last READ of the frontier's chunk, so a rule of "the frontier's chunk or the
next" took every late READ for a seek and cut the prefetches (the three-VM rig served 36
of 64 chunks from readahead with 14 cancelled). Readahead continues from the frontier,
so a late READ prefetches nothing the stream has passed. A READ that continues neither
stream (a seek) starts a stream in the empty slot or in place of the stream read least
recently, and cancels that stream's prefetches still in flight that no READ has joined:
their slots free at once and
nothing the client moved away from is fetched or cached; a joined prefetch is never
cancelled (`joined` is set under the reader's lock, where the cancel decides). `status`
counts them as `readahead_cancelled`. The first seek after a single stream takes the
empty slot and cancels nothing, which is the price of keeping a tail request harmless.

A client's own fetch stops when nobody wants it (I-314): the web UI reads a file under
the request's context (`vfs.File.ReadAtContext`), and when the browser abandons a
request (a seek or a closed player) the READ returns and, if no other READ waits on its
chunk fetch, the fetch is cut, so the chunk nobody wants stops taking the link from the
one the player asks for next. A READ that was served leaving never cuts the fetch (it
goes on to cache the chunk for the READs after it), and a whole-chunk reader (`ReadChunk`:
flusher, repair, pins) that joined a fetch keeps it going. NFS and SMB READs have no
request context and read through `ReadAt`.

## 6. Visibility

- **Issuing gateway:** read-your-writes immediately after the WRITE reply, from the overlay
  (WAL extents over base, with base bytes at or beyond `minSize` reading as zero).
  `GETATTR` size comes from the overlay while dirty, and so do its times: mtime and ctime
  are the file's last write or truncate on this gateway, or for mtime the time a client
  set after it (I-230), never earlier than the committed ones; the flush then stamps the
  leader's time (I-261). Until 2026-10-13 a write left the committed mtime in place until
  the flush, about 2 s later, so `make`, `rsync` and an NFS client's cache saw a file of
  the new size with the old time. The write time is this gateway's clock and the flush's
  the leader's, so a gateway whose clock runs ahead can see mtime step back slightly at
  the flush.
- **A file being written lands whole** (I-401, I-230). A copy creates a file, writes it
  and then sets its mode, owner and the source's times (`cp -p`, `rsync -a`, Finder,
  robocopy, Explorer's SET_INFO basic): measured on macOS, one `cp -p` of a 6-byte file
  sent 14 SETATTRs for the file and its AppleDouble `._` companion, each a Raft proposal
  to the leader before 2026-10-17. A SETATTR of mode, owner or times on a file this
  gateway holds unflushed data for proposes nothing: it is logged in the inode's WAL (an
  `attrs` record, fsynced before the reply, so an acknowledged SETATTR survives a crash
  like an acknowledged WRITE), this gateway's GETATTR shows it at once, and the flush's
  Commit carries it (`Commit.Attrs`, codec 16), applied after the Commit stamps its own
  mtime and ctime. A later write or truncate drops the pending times, being newer than
  they are, but keeps the mode and owner; a flush that fails keeps them pending, and a
  flush whose data is dropped (superseded, refused, the file gone) drops them with it.
  Other gateways see the new mode, owner and times with the data, at the flush (within
  the idle timer after the copy pauses, §3), and until then the create's. A SETATTR on a
  file with nothing dirty here, or one with a size, is proposed as before, and one that
  changes nothing proposes nothing at all (I-402). While a live member reads codec 15 or
  older the flusher proposes the pending attributes as a `SetAttr` right after the Commit
  (what every release before codec 16 did for times). Times set on another gateway than
  the one holding the dirty data are not noted there: that Commit still stamps over
  them. A time outside what the inode holds (nanoseconds since 1970 in an int64,
  1677–2262) is held at the nearest end, not wrapped (a FILETIME of 1601 had read back
  as 2185). Until 2026-10-12 every copied file showed the flush's time, so robocopy and
  rsync copied everything again on their next run.
- **Other gateways:** see the previous committed version until their FSM applies the Commit
  (milliseconds after Raft commit). They never see a torn mixture of old and new chunks,
  because the chunk list is replaced atomically by one Commit.
- **Two gateways writing the same file in place:** disjoint chunks → both Commits land
  (the second rebases, keeping the larger size); same chunk → the second Commit's chunk,
  re-assembled over the base as it is then (§4 step 10), replaces the first's: bytes
  the second gateway did not write in that chunk keep the first gateway's, so the merge
  is byte-exact for disjoint ranges and last-commit-wins for overlapping ones. A truncate
  rebased over a concurrent append wins over the append. This is documented, not hidden.
- **Two clients replacing the same file** (codec 9, I-223): a copy that replaces a file
  opens it truncated (`O_TRUNC`: SMB overwrite, overwrite-if or supersede, an NFS
  `UNCHECKED` CREATE) and writes it front to back, and two such copies at once must
  leave one whole version, never chunks of both. A writer's `O_TRUNC` open marks a
  **rewrite** on the inode, even of an empty file (where it records no truncate), and so
  does a plan whose truncation point is 0 (a SETATTR to size 0, a truncate replayed from
  the WAL). The inode's next flush is the **rewrite start**: the plan becomes a truncate
  to 0, its Commit carries `RewriteStart`, replaces the file whatever was committed since
  it was planned (02 §Apply rules) and returns the version it produced, which the
  gateway keeps as the inode's **rewrite id** (`Overlay.MarkRewrite`, `SetRewrite`).
  Every later Commit of the inode from that gateway carries the id; the FSM refuses one
  whose id is no longer the inode's `RewriteAt` with `ErrSuperseded`, because another
  rewrite started after this one, and the flusher then drops the inode's dirty extents
  and WAL (the client's copy ran, and the other replaced it, exactly as if they had run
  one after the other). The claim stays after that, so the rest of the same copy is
  refused the same way rather than mixed into the winner. The rewrite whose start
  applied last wins, whole. A plan truncating to 0 starts a new rewrite even while an
  older claim of this gateway holds the inode (a second copy over the same file). A claim
  lives while WRITEs of the inode keep reaching this gateway or its data is still dirty,
  and lapses `RewriteHold` (60 s) after the last WRITE once its data has landed: a copy
  whose writes pause, or whose flushes retry, stays one rewrite, and an in-place edit a
  minute later is a plain write again. A pending claim whose flush committed as a plain
  write (the gate below was closed, or the leader refused it) is gone: no later flush of
  that copy may start a rewrite, which would truncate what the plain one committed. A
  gateway restart forgets its claims (the WAL holds no commit results); a replayed
  truncate to 0 starts a new rewrite, and anything else commits as plain writes. Until
  every live member reads codec 9 the flusher sends neither field (it asks the prober's
  `MinCodec`: a codec-9 entry forwarded to an older leader is refused as malformed, not
  with `ErrCodec`), and the in-place rule above applies; a leader that still answers
  `ErrCodec` gets the same chunks again as a plain write.

## 7. When the cluster is unavailable

| State | WRITE RPC | Flush |
|---|---|---|
| Raft has a leader, ≥ k live storage nodes | OK | proceeds |
| Raft has a leader, < k live | OK until `MaxDirty`, then `NFS3ERR_IO` | fails at step 9, retried with backoff |
| no leader / `Health == Stale` | data WRITEs accepted into the WAL until `MaxDirty`, then `NFS3ERR_IO`; metadata mutations (create, mkdir, unlink, rename, setattr) `EROFS` (`NFS3ERR_ACCES` or `NFS3ERR_IO` on the wire, 07) | fails at step 10, retried |
| gateway process restart | — | WAL replayed, flush resumes |

The WAL is the only place acknowledged-but-unflushed bytes live. It is fsynced per record
and replayed on start, so neither a crash nor a long outage loses it.

## 8. Durability contract (normative, quoted verbatim in README and 05)

"No data loss" means: every byte whose WRITE was acknowledged is durable at once (fsynced
to the issuing gateway's WAL) and is returned unchanged by any later READ on the issuing
gateway immediately; it is returned unchanged by any later READ on **every** live gateway
once it has been flushed to the cluster, which happens within the idle timer (2 s) after
writes to that file pause, and otherwise within the dirty-byte thresholds (32 MiB per file,
256 MiB total, or a 128 MiB WAL) while writes continue. Every metadata op that returned OK
is visible on every live gateway once its Raft entry is applied there. This holds under:
(a) permanent loss of any **m** nodes' disks; (b) gateway process crash or power loss at any
instant, including right after an ack (WAL replay); (c) leader crash or partition at any
point in Apply; (d) corruption of up to m shards per stripe (detected by hash, repaired);
(e) kill -9 or power loss at any fsync step of a shard write (temp files swept, never
served; an acknowledged shard is fsynced with its directory); (f) loss of a Raft minority.
**Not covered:** > m nodes lost before repair; permanent loss of Raft majority; storage that
lies about fsync; > m corrupt shards in one stripe; destruction of the issuing gateway's
disk before its WAL has flushed (option for later: WAL mirroring to one peer before ack).
Degraded writes (user's choice) hold < m parity until repair.

### 8.1 fsync table

| Component | What is fsynced | When |
|---|---|---|
| WAL (`staging/<ino>.wal`) | the file; the directory when the file is created | after every appended record, before the WRITE reply (concurrent appends to one inode share one fsync, §2.1, I-168); the directory before the first record |
| shard store (`shards/hh/<key>`) | temp file, rename, directory | `write → f.Sync() → Close → Rename → fsync(dir)`; `Put` returns (and the PUT is acked) only after the directory fsync |
| raft log/stable (`raft/raft.db`) | bbolt | `NoSync = false` (every Raft append) |
| snapshots | `FileSnapshotStore` | fsync on `Close` |
| snapshot of FSM state | none beyond above | FSM is in memory; it is rebuilt from log + snapshot |

**No production path skips fsync and there is no test-mode variable.** Tests that need
speed use `raft.NewInmemStore` and an in-memory `sysfs.FS`; the WAL always fsyncs;
durability tests use real temp directories.

### 8.2 Future option: WAL mirroring

Closing the "issuing gateway's disk destroyed before flush" gap would mean sending each WAL record to
one live peer (`POST /v1/wal/<ino>`) before the WRITE reply, and replaying mirrored WALs
on the peer when the gateway does not come back. Cost: one tailnet round trip per WRITE.
It is deliberately not in the barebones scope; the seam is `WAL.AppendWrite`.

## 9. Memory model of the write path

The volume is used for media: a single file is routinely tens of GiB. Every stage of the
write path is bounded by a constant of the configuration, never by the file or by how much
of it is dirty. Dirty bytes live in the WAL on disk (§2); what the process holds is:

| Stage | Bytes in memory | Bound (defaults) | Released by |
|---|---|---|---|
| NFS requests in flight | each request's frame, read whole before its handler runs (docs/07, I-167) | connections × `InFlight` (16) × `MaxFrame` (1 MiB + 8 KiB) | the reply |
| web upload in flight | one chunk-sized buffer per upload | `uploadSlots` (4) × ChunkSize | the response (docs/13 §4) |
| WAL append | one record copy per WRITE in flight | connections × `InFlight` × wsize (docs/07) | its fsync |
| overlay bookkeeping | 24 B per extent; a WRITE past the last extent appends without copying the set (I-399) | stage / wsize extents (about 1.2 MiB at a 50 GiB stage and 1 MiB writes) | the Commit that cleans them |
| flush: assemble + encode | the chunk and its `(k+m)` shards, per chunk in the pipeline | `ChunkWorkers × (ChunkSize + (k+m) · ShardLen(ChunkSize, k))` = 8 × 10 MiB = **80 MiB** at 4 MiB and (2,1) | the chunk being decided (step 9) |
| compaction | one streaming buffer, over at most `CompactMax` live bytes | 1 MiB | the rewrite |
| shard RPC server | the whole body of each in-flight PUT or GET | `slots × MaxShard ≤ 256 MiB` (32 at 4 MiB, 4 at 64 MiB) | the reply |
| read cache, memory tier | hot decoded chunks | 64 MiB | LRU |
| read cache, disk writes | chunks whose file is still being written behind the Put (01 §Local chunk cache, I-204) | `CacheQueue × ChunkSize` = 8 × 4 MiB = **32 MiB** | the write |
| readahead | the chunk and its k shards per prefetch (§5) | `ReadAhead × (ChunkSize + k · ShardLen(ChunkSize, k))` = 4 × 8 MiB = **32 MiB** at 4 MiB and (3,2); a stream holds at most `ReadaheadDepth(ChunkSize)` of them, 2 at 4 MiB (I-203) | the cache Put |
| FSM | the chunk list of every file, 32 B per chunk | docs/01 | – |

`Flusher.Buffers()` reports the flush row live and its peak since start; `status` shows it
with the overlay's plan bytes and WAL bytes (docs/12), and the soak suite's large-file
phase (docs/09 §6) asserts the row's bound and a flat heap while writing a file many times
the WAL cap.

## Invariants

- **I-7** — A WRITE is acknowledged only after its bytes are fsynced to the WAL, and a Commit is proposed only after ≥ k shards of every new chunk are durable (file and directory fsynced) on distinct nodes; the Commit becomes visible only after Raft commit.
- **I-11** — A shard file visible under its final name has content matching its header hash; temp files are never served.
- **I-34** — The issuing gateway returns its own acknowledged bytes on any subsequent READ (read-your-writes), before and after flush.
- **I-35** — A flush never drops dirty extents or a truncation point, save on `ErrSuperseded` (I-223): on `ErrConflict`, `ErrUnknownChunk` or PUT failure the same plan (extents, `minSize`, `sizeSet`) is retried against the newer base with `NewSize` recomputed so that no other gateway's committed bytes are cut off except by an explicit truncate; extents are removed only after `WaitApplied` of the Commit that carries them.
- **I-312** — A retry of a flush (after a failed or unknown Commit, a conflict rebase, or
  a backoff) never re-uploads a chunk it placed within `ReuseFor` (half the orphan grace,
  5 min by default): it HEADs the chunk's slots and PUTs only the missing ones, keeping the
  placement's original age, so the sweep cannot take a reused shard; past `ReuseFor` the
  retry PUTs again (L-58). A flush that keeps failing on a slow link therefore costs a
  Commit per attempt, not the file (on the real cluster each retry of a 44 MB flush had
  re-PUT 73 MB and saturated the gateway's uplink, which failed the next Commit).
- **I-36** — Other gateways observe either the previous committed chunk list or the new one, never a mixture.
- **I-223** — Two rewrites of one file leave one of them whole: a writer's `O_TRUNC`
  open, or a plan truncating to 0, makes the inode's next flush a truncate to 0 that
  commits with `RewriteStart`, which applies over any version and makes the version it
  produces the inode's `RewriteAt` (returned in `Result.Version`); every later Commit of
  that inode from the same gateway carries that id until `RewriteHold` passes without a
  WRITE of the inode and with no dirty data of it, a new truncate to 0 starts another,
  or the gateway restarts; a copy whose first flush committed plain starts none; a Commit whose id is not the inode's
  `RewriteAt` is `ErrSuperseded` and changes nothing, and its flusher drops the inode's
  dirty extents and WAL, keeps the claim and counts it in `gateway.rewrites_superseded`;
  so after two clients each truncate one path and write a payload through two gateways,
  in any interleaving of their flushes, every gateway reads exactly one of the payloads
  whole and every FSM holds the same state; a plain Commit neither starts nor ends a
  rewrite; the fields are sent only while every live member reads codec 9.
- **I-37** — WAL replay after a crash restores exactly the records whose CRC verifies, in order, and discards the torn tail; replayed bytes equal the acknowledged bytes.
- **I-63** — A WRITE is acknowledged only when every file its durability depends on has a durable directory entry: a newly created WAL file's directory is fsynced before its first record is appended.
- **I-78** — The bytes a gateway holds in memory for flushing are bounded independently of file and plan size: at most `ChunkWorkers` chunks are assembled at once, a chunk's buffer and shards are released once its PUTs are decided (acked, failed or cancelled), a Commit retry re-assembles from the WAL, and `Flusher.Buffers()` never exceeds `ChunkWorkers × (ChunkSize + (k+m) · ShardLen(ChunkSize, k))`.
- **I-79** — A WRITE that meets the per-inode WAL cap or the gateway dirty-byte cap waits for the flusher up to `CapWait` and succeeds as soon as the cap clears; it fails with `EIO` only when `CapWait` expires, and with `ENOSPC` at once only when that inode's last flush attempt failed because the cluster is full; any other flush failure is waited out; acknowledged bytes are never affected by the wait or the failure.
- **I-80** — A flush commits at most `FileDirtyHi` bytes of chunks per Commit and leaves the rest dirty as residual; the gateway's total dirty count includes the bytes of outstanding plans; compaction streams live records through a bounded buffer and leaves the inode on an open, correctly indexed WAL even when a step after the rename fails; a per-inode log never exceeds `WALCap` plus one record and one segment.
- **I-396** — The stage (`MaxDirty`) is `staging.size` when set, otherwise a quarter of the free bytes of the staging directory's filesystem at open, the bytes its logs hold counted as free, and never under 1 GiB; `WALCap` is the stage, never under 128 MiB; a WRITE also waits (I-79) while that filesystem's free bytes are under `StageReserve` (2 % of its size, at least 1 GiB), read at most once a second.
- **I-397** — A committed flush appends a `clean` record and copies no record: after it the log replays to exactly the overlay's state, the segments wholly below the lowest live extent are removed oldest first, and a file written and flushed front to back keeps a log of at most its dirty bytes plus two segments; a compaction runs only when the dead bytes reach the live bytes plus `CompactMin` and the live bytes are at most `CompactMax`, so no WRITE waits on a copy of more than `CompactMax` bytes; a crash at any step of an append, a rotation, a clean, a removal or a compaction replays to the acknowledged bytes (I-37).
- **I-398** — `status` reports `gateway.staged` (dirty and plan bytes), `gateway.stage`, `gateway.flushed_bytes` (cumulative bytes the flusher committed) and `gateway.drain_rate` (bytes a second over the last minute); the CLI and the dashboard show the staged bytes with the time to drain at that rate, and `check` warns while more than 1 GiB is staged.
- **I-399** — An extent insert past the last extent appends in place: a run of sequential WRITEs costs O(1) amortised each, never a copy of the extent set.
- **I-83** — A shard PUT's deadline scales with its size, the RPC server's concurrent PUT and GET slots satisfy `slots × MaxShard ≤ 256 MiB`, and a PUT whose slot was re-placed or whose chunk has been decided is cancelled and awaited, so no goroutine keeps a shard buffer alive past its chunk.
- **I-98** — A chunk read returns as soon as k shards have arrived and cancels every GET
  it still has in flight (the hedged request and any slower holder), so a stuck holder
  never keeps a shard buffer or a server slot past the read that asked.
- **I-101** — After a failed fsync the WAL reopens its file before the next append, so a
  later fsync cannot report success for bytes the kernel already dropped; after a
  compaction or reopen that fails, every call on that WAL returns `ErrWALClosed` until the
  gateway restarts and replays the log, and replay reports the bytes it discarded after
  the last intact record.
- **I-169** — After a chunk's k-th ack the flusher waits for its remaining PUTs at most
  the straggler grace, `clamp(4 × p95 of recent PUT durations, StragglerMin, PutDeadline)`,
  and a PUT still pending then is cancelled, awaited and its slot re-placed or recorded 0
  exactly as at the deadline, its host held slow (I-371); the wait for the first k acks is
  unchanged.
- **I-171** — A shard GET's deadline for the reply body is `callTimeout(Content-Length)`
  from the reply's own headers, never the `MaxPayload` allowance; a reply announcing more
  than `MaxPayload + HeaderSize` bytes, or no length, is refused before any of it is read.
- **I-168** — Concurrent appends to one inode's WAL share fsyncs: an append returns
  success only after an fsync that began after its record was written has succeeded, one
  fsync may cover any number of records, a record's in-memory effect is applied only after
  that fsync and in log order, a failed fsync fails every append it covered and cuts the
  log back to the last fsynced byte before any later append, and no flush snapshot,
  compaction or eviction runs while an append of that inode is between its write and its
  fsync.
- **I-166** — Readahead is bounded and invisible: at most `ReadAhead` prefetches are in
  flight gateway-wide (`Reader.Prefetching()` never exceeds it), a candidate that finds
  every slot busy is skipped and counted in `readahead_skipped`, a later READ of the same
  chunk retrying it, a prefetch is started only by a sequential READ (one in the chunk of
  the previous READ or the next), never for `HashZero`, a cached chunk or one already in flight,
  shares the in-flight table with foreground reads so no chunk is fetched twice, holds no
  bytes beyond its own fetch (the result goes only to the cache), and its failure changes
  nothing a client sees.
- **I-201** — A chunk's `Sums` are the SHA-256 of each of its k data shards' stored
  payload, a pure function of the chunk's bytes and `(k, m)`: the flusher computes them
  from the shards it encodes and the `Commit` carries them while every live member reads
  codec 5, otherwise the chunk is committed without them and the leader's backfill
  records them with `SetShardSums` after fetching, decoding and verifying the chunk, at
  most `SumsBatch` chunks a pass; a `SetPlacement` to another `(k, m)` clears them; a
  chunk never carries sums that differ from the ones another node computes, and `status`
  reports the chunks still lacking them.
- **I-203** — Readahead reaches `ReadAheadBytes` (8 MiB) ahead of a sequential client: a
  stream's depth is `ReadaheadDepth(ChunkSize)` = `min(ReadAhead, ReadAheadBytes /
  ChunkSize)`, at least one chunk; a prefetch starts only after the READ that triggered
  it has been served; its shard GETs carry the background mark (I-175); an inode has two
  streams, each with a frontier (the furthest chunk its READs reached), a READ within
  `ReadAhead` chunks of a frontier on either side continues that stream from its
  frontier (a client's concurrent READs are served out of order), and a READ that
  continues neither replaces the stream read least recently (or takes an
  empty slot) and cancels that stream's in-flight prefetches no READ has joined, never the
  other stream's, counted in `status` as `readahead_cancelled`, while a joined prefetch is never
  cancelled; a READ that joined a prefetch which then failed fetches the chunk itself
  and returns only its own result.
- **I-314** — A client READ whose context ends (the web UI's request abandoned by the
  browser) returns at once, and the chunk fetch it waited on is cut when no other READ
  waits on it: an abandoned fetch caches nothing; a fetch another READ or a whole-chunk
  reader still waits on goes on and serves it; a served READ never cuts its fetch; a
  prefetch is cut only by I-203.
- **I-310** — A finished prefetch is held by the chunk cache alone: the readahead table
  drops a prefetch the moment it completes, joined or not, so the bytes a gateway keeps
  for readahead are bounded by the prefetches in flight (`ReadAhead`) and the cache's
  memory tier, never by how many files' heads were read (on the real cluster, the web UI
  reading thousands of file heads had kept 6.7 GB of decoded chunks alive on one node).
- **I-261** — While a file has unflushed writes on a gateway, that gateway reports its
  mtime and ctime as the time of its last write or truncate there (the mtime a client set
  after it, when one did), never earlier than the committed times, as it reports the
  overlay's size; after the flush they are the Commit's.
- **I-230** — Times a client sets on a file (`Chtimes`: SMB SET_INFO basic, NFS SETATTR)
  after its last write and while that write is unflushed are the file's times after the
  flush; a write or truncate after them makes the flush's time win; a time before
  1677-09-21 or after 2262-04-11 reads back as that bound, never as another date.
- **I-401** — A SETATTR of mode, owner or times on a file the gateway holds unflushed data
  for proposes nothing: it is fsynced in the inode's WAL before the reply, shown by that
  gateway at once, replayed after a restart, and committed by the flush (`Commit.Attrs`,
  codec 16, or a `SetAttr` after the Commit while a live member reads an older codec);
  a later write or truncate drops its times and keeps its mode and owner; an aborted
  flush keeps it pending and a dropped one drops it.
- **I-402** — A SETATTR that changes nothing the inode holds proposes nothing.
- **I-403** — `status` reports `gateway.proposals`, the proposals this gateway's filesystem
  made since start by op, and `gateway.attrs_deferred`, the SETATTRs it held for a flush.
- **I-219** — A Commit the leader refuses with `ErrCodec` (its view of the members' codec
  is older than the gateway's, as during a rolling upgrade) is built again without
  `Sums` and proposed again within the same flush attempt, so the codec gate never fails
  a flush and never leaves the inode's last flush in error; the chunks carry no sums and
  the leader's backfill records them once every live member reads codec 5 (06 §4); while
  a live member reads less, a repair pass backfills nothing, fetching no chunk for a
  `SetShardSums` the leader would refuse.

## Tests

| Invariant | Tests |
|---|---|
| I-7 | `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes`, `TestCrash_AtEveryPoint`, `TestFlusher_FewerThanKAcks_StaysDirty`, `TestFlusher_LateRetry_RePutsBeforeCommit`, `TestFlusher_PartialFlush_TwoExtents_KeepsPartialChunks`, `TestFlusher_PartialFlush_UnalignedAppend_KeepsHeadChunk`, `TestFlusher_SmallAndLargeChunksCommit`, `TestMount_Workload`, `TestNFS_WriteSequence_MultiRPC`, `TestOverlay_RestartReplaysDirtyWALs`, `TestOverlay_TruncateUpDuringFlush_SurvivesReopen`, `TestOverlay_WriteNotAckedWhenFsyncFails`, `TestVFS_PerRPCOpenCloseSequence`, `TestVFS_WriteToSymlinkInode_NotSilentlyDropped`, `TestWAL_AppendReplayRoundTrip`, `TestWAL_CreateFsyncsDirectory`, `TestWAL_SyncFailure_ReturnsError` |
| I-11 | `TestCache_OpenRemovesTempFiles`, `TestChunkRPC_PutGetHeadDelete_SizeBoundaries`, `TestStore_CrashAfterRenameBeforeDirSync_ShardStillValid`, `TestStore_CrashBeforeRename_LeavesNoVisibleShard`, `TestStore_GetVerifiesHash_Corrupted`, `TestStore_PayloadBoundaries`, `TestStore_PutDifferentPayloadSameKey_Replaces`, `TestStore_PutGetRoundTrip`, `TestStore_TornWrite_DetectedOnRead` |
| I-34 | `TestExtents_InsertOverlap_Property`, `TestMount_Workload`, `TestNFS_WriteSequence_MultiRPC`, `TestOverlay_OracleRandomOps`, `TestOverlay_TruncateDuringFlush_HidesPlanBytes`, `TestOverlay_WriteAtNonZeroOffset_PreservesBase`, `TestVFS_MaxDirty_ReturnsEIO`, `TestVFS_OracleRandomOps`, `TestVFS_PerRPCOpenCloseSequence`, `TestVFS_WriteAtNonZeroOffset_PreservesPrefix` |
| I-35 | `TestCluster_StaleGatewayOverlay_KeepsOtherGatewaysBytes`, `TestFlusher_ConflictRebase_PreservesConcurrentAppend`, `TestFlusher_ConflictRebase_TruncateWins`, `TestFlusher_PartialFlush_SizeNeverAheadOfData`, `TestFlusher_PartialFlush_TwoExtents_KeepsPartialChunks`, `TestFlusher_PartialFlush_UnalignedAppend_KeepsHeadChunk`, `TestFlusher_ProposeFailureRetriesSamePlan`, `TestOverlay_AbortFlush_MergesBackNewerWins`, `TestOverlay_StaleEntry_FollowsCommittedSize`, `TestVFS_OracleRandomOps`, `TestVFS_TruncateThenGrow_OldBytesDoNotResurface`, `TestWAL_Compact_KeepsOnlyLive` |
| I-36 | `TestCluster_OtherGatewaySeesPreviousVersionUntilApply`, `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes` |
| I-223 | `TestFSM_Commit_Rewrite_Table`, `TestEncodeCommand_RewriteIsCodec9`, `TestResult_VersionRoundTrip`, `TestCodec_ReadsVersion7Snapshot`, `TestFlusher_Superseded_DropsDirty`, `TestFlusher_Rewrite_GatedOnMinCodec`, `TestCluster_TwoRewrites_OneWholeWins`, `TestCluster_Codec9RefusedWhileMemberOld`, `TestSoak_MultiUserAgainstHarness` |
| I-37 | `FuzzWALReplay`, `TestOverlay_CommitFlushed_PostRenameError_OffsetsValid`, `TestWAL_Compact_SyncDirFails_LogStillUsable`, `TestOverlay_OracleRandomOps`, `TestOverlay_RestartReplaysDirtyWALs`, `TestOverlay_StaleCompactTempRemoved`, `TestOverlay_TruncateUpDuringFlush_SurvivesReopen`, `TestWAL_AppendAfterFailedSync_NoResurrectedRecord`, `TestWAL_AppendReplayRoundTrip`, `TestWAL_Compact_KeepsOnlyLive`, `TestWAL_RecordSizeBoundaries`, `TestWAL_ReplayStopsAtTornTail` |
| I-63 | `TestWAL_CreateFsyncsDirectory` |
| I-78 | `TestFlusher_Buffers_BoundedByChunkWorkers`, `TestFlusher_ConflictRetry_ReassemblesFromWAL`, `TestFlusher_DoesNotFillCache` |
| I-169 | `TestFlusher_StragglerGrace_CutsWaitAfterKAcks`, `TestFlusher_StragglerGrace_Table`, `TestFlusher_StragglerGrace_NotBeforeKAcks` |
| I-171 | `TestClient_GetDeadline_FromContentLength` |
| I-168 | `TestWAL_GroupCommit_OneFsyncCoversConcurrentAppends`, `TestWAL_GroupCommit_FailedFsyncFailsEveryWaiter`, `TestWAL_GroupCommit_LateAppendWaitsForNextFsync`, `TestOverlay_PendingWrite_NotVisibleUntilFsynced`, `TestOverlay_FailedGroupFsync_DropsEveryPendingWrite`, `TestOverlay_CommitFlushed_WaitsForPendingAppend`, `TestOverlay_WriteDoesNotHoldInodeLockDuringFsync`, `TestOverlay_TruncateDuringPendingAppend_MatchesReplay` |
| I-201 | `TestFSM_Commit_SumsPureFunctionOfChunk`, `TestFSM_SetShardSums_Table`, `TestFSM_SetPlacement_PolicyChangeClearsSums`, `TestFlusher_CommitCarriesSumsOnlyAtCodec5`, `TestRepair_BackfillSums_Bounded`, `TestCluster_V5CommitWithoutSumsWhileMemberOld`, `TestCodec_ReadsVersion4Snapshot`, `TestCodec_StateRoundTrip`, `TestEncodeCommand_Golden`, `TestEncodeCommand_V5Gating`, `FuzzDecodeCommand` |
| I-219 | `TestFlusher_CommitRefusedForCodec_CommitsWithoutSums`, `TestRepair_BackfillSums_WaitsForCodec5` |
| I-310 | `TestReader_Readahead_FinishedPrefetchNotRetained` |
| I-312 | `TestFlusher_SoonRetry_ReusesPlacedShards`, `TestFlusher_LateRetry_RePutsBeforeCommit` |
| I-314 | `TestReader_AbandonedReadCutsItsFetch`, `TestWithContext_ReadsUnderTheRequestContext` |
| I-203 | `TestReader_Readahead_TwoStreamsKeepTheirPrefetches`, `TestReader_Readahead_DepthByBytes_Table`, `TestReader_Readahead_PrefetchIsBackgroundPriority`, `TestReader_Readahead_RandomReadCancelsUnjoinedPrefetch`, `TestReader_Readahead_JoinedPrefetchSurvivesCancel`, `TestFile_ReadAt_PrefetchStartsAfterTheRead`, `TestReader_Readahead_ReadInSameChunkKeepsPrefetches`, `TestReader_JoinedPrefetchFailure_ReadFetchesItself` |
| I-166 | `TestReader_Readahead_SequentialFillsCache`, `TestReader_Readahead_RandomReadDoesNotPrefetch`, `TestReader_Readahead_BoundedInFlight`, `TestReader_Readahead_ForegroundJoinsPrefetch`, `TestReader_Readahead_SkipsHashZeroAndCached`, `TestReader_Readahead_FailureIsInvisible`, `TestPhaseHotPath_ConformingGatewayPasses`, `TestPhaseHotPath_NonConformingGatewayFails`, `TestPhaseHotPath_OneGatewayUnmovedCountersSkip`, `TestReader_Readahead_SameChunkReadRetriesSkippedCandidates` |
| I-79 | `TestFlusher_FullCluster_ErrorWrapsErrFull`, `TestOverlay_WALCap_WaitsForFlush`, `TestOverlay_WALCap_FailsAfterCapWait`, `TestOverlay_WALCap_FailsFastWhenClusterFull`, `TestNFS_WriteAtCap_WaitsThenSucceeds`, `TestNFS_WriteWhenFull_IsNOSPC`, `TestVFS_ClusterFull_ENOSPC` |
| I-80 | `TestFlusher_BatchCommit_ResidualStaysDirty`, `TestOverlay_TotalDirty_CountsPlanBytes`, `TestOverlay_TotalDirty_FollowsAbortAndDrop`, `TestWAL_Compact_StreamsThroughBoundedBuffer`, `TestWAL_Compact_SyncDirFails_LogStillUsable`, `TestOverlay_CommitFlushed_PostRenameError_OffsetsValid` |
| I-396 | `TestOverlay_Stage_Derived`, `TestOverlay_Stage_CountsOwnLogs`, `TestOverlay_Stage_Boundaries`, `TestOverlay_StageReserve_Waits`, `TestConfig_StagingSize` |
| I-397 | `TestWAL_Clean_ReplaysToOverlay`, `TestWAL_Sequential_DropsSegments`, `TestWAL_Rotate_CrashPoints`, `TestWAL_Compact_OnlySmallLive`, `TestOverlay_LargeStage_NoCopy`, `TestOverlay_ReplayModel` |
| I-398 | `TestGatewayReport_Staged`, `TestFlusher_DrainRate`, `TestCheck_WarnsStaged` |
| I-399 | `TestExtents_AppendInPlace` |
| I-83 | `TestFlusher_StragglerPut_CancelledAfterReplace`, `TestClient_PutTimeout_Table`, `TestNode_SlotBudget_Table`, `TestChunkRPC_PutCancelled_NoShardLeft` |
| I-98 | `TestReader_HedgesStalledHolder` |
| I-261 | `TestVFS_DirtyFile_ReportsItsWriteTime` |
| I-230 | `TestVFS_UserTimes_SurviveFlush`, `TestSMB_CopyTimes_SurviveFlush` |
| I-401 | `TestStub_I_401` |
| I-402 | `TestStub_I_402` |
| I-403 | `TestStub_I_403` |
| I-101 | `TestWAL_FsyncFailureReopens`, `TestWAL_CompactReopenFailureClosesLog` |
