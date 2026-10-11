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

One file per dirty inode at `<data_dir>/staging/<ino>.wal`, append-only:

```
record := crc32c u32 | type u8 | off u64 | len u32 | data[len]
type    := 1 = write (data follows), 2 = truncate (len = 0, off = new size)
```

`crc32c` covers `type|off|len|data`. All integers little-endian.

```go
type WAL struct { fs sysfs.FS; f sysfs.File; off int64 }
func openWAL(fs sysfs.FS, path string) (*WAL, error)                       // a new file's directory entry is fsynced before it is used
func (w *WAL) AppendWrite(off uint64, p []byte) (walOff int64, err error)   // write + f.Sync()
func (w *WAL) AppendTruncate(size uint64) error                             // + f.Sync()
func (w *WAL) ReadAt(p []byte, walOff int64) error
func (w *WAL) Replay(fn func(typ uint8, off uint64, data []byte, walOff int64) error) error
func (w *WAL) Compact(live extents) (extents, error)                        // rewrite to a new file holding only live extents, fsync, rename
```

Rules:

- `AppendWrite` and `AppendTruncate` return only after `f.Sync()` succeeds. There is no
  "no-sync" mode: the struct has no such field, and tests that want speed put the staging
  directory on a tmpfs or accept the cost. All file access goes through the `sysfs.FS`
  seam so tests can inject a failed fsync or a crash at any step.
- A WAL file is created on the first write to an inode, and its directory is fsynced before
  the first record is appended: an acknowledged record never depends on a directory entry
  that power loss could drop (I-63). After a failed fsync the un-synced record is cut back
  off the file so a later crash cannot replay bytes that were never acknowledged (I-37).
- A crash during `Compact` may leave `<ino>.wal.new`; `OpenOverlay` removes it (the old
  log is a superset and replays correctly).
- `Replay` reads records in order and **stops at the first record whose CRC fails or whose
  length runs past EOF** (the torn tail from a crash mid-append). Everything before it is
  replayed into the extent set; the file is then truncated to that point.
- `Reset` happens when the extent set is empty after a successful flush. If the fresh
  extent set is non-empty (writes arrived during the flush), `Compact` rewrites the WAL to
  hold only the live extents, so a continuously written file never accumulates a WAL of
  every byte ever written. The rewrite streams each live record from the old file to the
  new one through one bounded buffer (1 MiB), never the whole live set at once, and reopens
  the renamed file before it reports success: a failure after the rename leaves the inode
  on the new file with correct record offsets, never on a closed handle (I-80).
- A per-inode WAL larger than `4 × FileDirtyHi` (128 MiB by default) forces a flush of that
  inode regardless of the idle timer.
- `OpenOverlay` replays every `staging/*.wal` at startup, so a gateway that crashed with
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
| inode WAL file ≥ 4 × `FileDirtyHi` | 128 MiB | full flush of that inode (its WAL compacts); a WRITE that arrives while the WAL is at the cap **waits** (next row) |
| total dirty bytes ≥ `TotalDirtyHi` | 256 MiB | flush the largest inodes until below |
| total dirty bytes ≥ `MaxDirty` (fresh **and** in-flight plan bytes) | 1 GiB | a WRITE **waits** (next row) while the overlay holds `MaxDirty` or more dirty bytes; existing dirty data is intact and drains through the triggers above |
| a WRITE meets the WAL cap or `MaxDirty` | `CapWait` 20 s | the WRITE blocks until the flusher frees room (a Commit applied, an inode evicted) and then proceeds; it fails only when `CapWait` expires (`NFS3ERR_IO`, `EIO`) or when that inode's last flush attempt failed (`EIO` at once, `ENOSPC` when the cluster was full), so a client that outruns replication is throttled to it rather than told the disk is broken (I-79). The waiting WRITE holds one of its connection's `InFlight` request slots (docs/07, I-167); `CapWait` is shorter than the clients' soft-mount timeouts (docs/07) |
| shutdown | 30 s (`ShutdownFlush`) | `FlushAll` with that deadline, then exit; whatever did not flush stays in the WAL and flushes on the next start |

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
   deadline, and its node is marked *suspect* (docs/05 §6) so the next chunk places
   around it instead of waiting on it again (I-169, L-209). The floor keeps a 256 KiB/s
   DERP peer (1.4 s for a 350 KiB shard) in the stripe; the multiplier follows a slow
   cluster. Slots still missing are recorded as `0` (degraded). If acks < k → return an
   error; the plan stays dirty and is retried with backoff (1 s doubling to 60 s); when
   every failed slot was `ErrFull` the error wraps `chunk.ErrFull` so the client sees
   `ENOSPC` rather than `EIO` (I-79). An ack means the receiver fsynced file and directory
   (I-7). Steps 4–9 run per chunk as one pipeline under a flusher-wide semaphore of
   `ChunkWorkers`: the chunk's bytes and shards are released the moment it is decided, and
   nothing of it but `(idx, hash, k, m, placement)` survives into step 10 (I-78). The flush
   does not populate the read cache; the first read after a flush fetches shards and the
   `Reader` caches then, so a bulk upload never evicts the hot set.
10. **Commit.** A plan is committed in **batches** of at most `FileDirtyHi` bytes of chunks
    (8 at 4 MiB): `assemble` keeps the lowest-indexed touched chunks that fit and leaves
    the rest dirty as residual, exactly like a partial flush; a plan with a truncate is
    never split (I-80, L-191). Then `Propose(Commit{Ino, BaseVersion: base.Version,
    HasTruncate: plan.sizeSet, TruncateTo: plan.minSize, NewSize, Writes, NewChunks, Now})`
    where `NewSize` is:
    - full flush, no truncate: `max(base.Size, maxExtentEnd)`;
    - full flush with truncate: `max(plan.minSize, maxExtentEnd)`;
    - partial or batched flush: `max(base.Size, min(end of the last committed chunk,
      plan.size))`; the dirty tail beyond it stays in the overlay and is committed by the
      next flush, so the committed size is never ahead of committed data.
    Error handling:
    - `ErrConflict`: another gateway committed first. Reload `base`, recompute steps 3–9 for
      the same plan (dedup makes untouched chunks free), propose again with `NewSize`
      recomputed from the *new* base by the rules above: another gateway's append is
      preserved, and our truncate still wins over it (last-writer-wins at chunk granularity
      is the documented behaviour of concurrent writers). Dirty extents are never dropped on
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
5. `codec.Decode(shards, c.Size)`; verify `sha256 == h`. Mismatch → `EIO`; the scrubber
   finds the bad shard (06 §6).
6. `cache.Put(h, chunk)`.

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
is documented to queue behind foreground traffic for that one chunk. And a READ of the
inode that leaves the sequence (a seek) cancels the inode's prefetches still in flight
that no READ has joined: their slots free at once and nothing the client moved away from
is fetched or cached; a joined prefetch is never cancelled (`joined` is set under the
reader's lock, where the cancel decides). `status` counts them as `readahead_cancelled`.

## 6. Visibility

- **Issuing gateway:** read-your-writes immediately after the WRITE reply, from the overlay
  (WAL extents over base, with base bytes at or beyond `minSize` reading as zero).
  `GETATTR` size comes from the overlay while dirty.
- **Other gateways:** see the previous committed version until their FSM applies the Commit
  (milliseconds after Raft commit). They never see a torn mixture of old and new chunks,
  because the chunk list is replaced atomically by one Commit.
- **Two gateways writing the same file:** disjoint chunks → both Commits land (the second
  rebases, keeping the larger size); same chunk → the second Commit wins for that whole
  chunk and the first committer's version of it is tombstoned; a truncate rebased over a
  concurrent append wins over the append. This is documented, not hidden.

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
| overlay bookkeeping | 24 B per extent | WALCap / wsize extents per inode | compaction |
| flush: assemble + encode | the chunk and its `(k+m)` shards, per chunk in the pipeline | `ChunkWorkers × (ChunkSize + (k+m) · ShardLen(ChunkSize, k))` = 8 × 10 MiB = **80 MiB** at 4 MiB and (2,1) | the chunk being decided (step 9) |
| compaction | one streaming buffer | 1 MiB | the rewrite |
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
- **I-35** — A flush never drops dirty extents or a truncation point: on `ErrConflict`, `ErrUnknownChunk` or PUT failure the same plan (extents, `minSize`, `sizeSet`) is retried against the newer base with `NewSize` recomputed so that no other gateway's committed bytes are cut off except by an explicit truncate; extents are removed only after `WaitApplied` of the Commit that carries them.
- **I-36** — Other gateways observe either the previous committed chunk list or the new one, never a mixture.
- **I-37** — WAL replay after a crash restores exactly the records whose CRC verifies, in order, and discards the torn tail; replayed bytes equal the acknowledged bytes.
- **I-63** — A WRITE is acknowledged only when every file its durability depends on has a durable directory entry: a newly created WAL file's directory is fsynced before its first record is appended.
- **I-78** — The bytes a gateway holds in memory for flushing are bounded independently of file and plan size: at most `ChunkWorkers` chunks are assembled at once, a chunk's buffer and shards are released once its PUTs are decided (acked, failed or cancelled), a Commit retry re-assembles from the WAL, and `Flusher.Buffers()` never exceeds `ChunkWorkers × (ChunkSize + (k+m) · ShardLen(ChunkSize, k))`.
- **I-79** — A WRITE that meets the per-inode WAL cap or the gateway dirty-byte cap waits for the flusher up to `CapWait` and succeeds as soon as the cap clears; it fails (`EIO`, or `ENOSPC` when the inode's last flush error was the cluster being full) only when `CapWait` expires or that inode's last flush attempt failed; acknowledged bytes are never affected by the wait or the failure.
- **I-80** — A flush commits at most `FileDirtyHi` bytes of chunks per Commit and leaves the rest dirty as residual; the gateway's total dirty count includes the bytes of outstanding plans; compaction streams live records through a bounded buffer and leaves the inode on an open, correctly indexed WAL even when a step after the rename fails; a per-inode WAL file never exceeds `WALCap` plus one record.
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
  exactly as at the deadline, its node marked suspect; the wait for the first k acks is
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
  flight gateway-wide (`Reader.Prefetching()` never exceeds it), a prefetch is started only
  by a sequential READ, never for `HashZero`, a cached chunk or one already in flight,
  shares the in-flight table with foreground reads so no chunk is fetched twice, holds no
  bytes beyond its own fetch (the result goes only to the cache), and its failure changes
  nothing a client sees.
- **I-203** — Readahead reaches `ReadAheadBytes` (8 MiB) ahead of a sequential client: a
  stream's depth is `ReadaheadDepth(ChunkSize)` = `min(ReadAhead, ReadAheadBytes /
  ChunkSize)`, at least one chunk; a prefetch starts only after the READ that triggered
  it has been served; its shard GETs carry the background mark (I-175); and a READ of
  the inode that leaves the sequence cancels the inode's in-flight prefetches no READ has
  joined, counted in `status` as `readahead_cancelled`, while a joined prefetch is never
  cancelled.

## Tests

| Invariant | Tests |
|---|---|
| I-7 | `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes`, `TestCrash_AtEveryPoint`, `TestFlusher_FewerThanKAcks_StaysDirty`, `TestFlusher_LateRetry_RePutsBeforeCommit`, `TestFlusher_PartialFlush_TwoExtents_KeepsPartialChunks`, `TestFlusher_PartialFlush_UnalignedAppend_KeepsHeadChunk`, `TestFlusher_SmallAndLargeChunksCommit`, `TestMount_Workload`, `TestNFS_WriteSequence_MultiRPC`, `TestOverlay_RestartReplaysDirtyWALs`, `TestOverlay_TruncateUpDuringFlush_SurvivesReopen`, `TestOverlay_WriteNotAckedWhenFsyncFails`, `TestVFS_PerRPCOpenCloseSequence`, `TestVFS_WriteToSymlinkInode_NotSilentlyDropped`, `TestWAL_AppendReplayRoundTrip`, `TestWAL_CreateFsyncsDirectory`, `TestWAL_SyncFailure_ReturnsError` |
| I-11 | `TestCache_OpenRemovesTempFiles`, `TestChunkRPC_PutGetHeadDelete_SizeBoundaries`, `TestStore_CrashAfterRenameBeforeDirSync_ShardStillValid`, `TestStore_CrashBeforeRename_LeavesNoVisibleShard`, `TestStore_GetVerifiesHash_Corrupted`, `TestStore_PayloadBoundaries`, `TestStore_PutDifferentPayloadSameKey_Replaces`, `TestStore_PutGetRoundTrip`, `TestStore_TornWrite_DetectedOnRead` |
| I-34 | `TestExtents_InsertOverlap_Property`, `TestMount_Workload`, `TestNFS_WriteSequence_MultiRPC`, `TestOverlay_OracleRandomOps`, `TestOverlay_TruncateDuringFlush_HidesPlanBytes`, `TestOverlay_WriteAtNonZeroOffset_PreservesBase`, `TestVFS_MaxDirty_ReturnsEIO`, `TestVFS_OracleRandomOps`, `TestVFS_PerRPCOpenCloseSequence`, `TestVFS_WriteAtNonZeroOffset_PreservesPrefix` |
| I-35 | `TestCluster_StaleGatewayOverlay_KeepsOtherGatewaysBytes`, `TestFlusher_ConflictRebase_PreservesConcurrentAppend`, `TestFlusher_ConflictRebase_TruncateWins`, `TestFlusher_PartialFlush_SizeNeverAheadOfData`, `TestFlusher_PartialFlush_TwoExtents_KeepsPartialChunks`, `TestFlusher_PartialFlush_UnalignedAppend_KeepsHeadChunk`, `TestFlusher_ProposeFailureRetriesSamePlan`, `TestOverlay_AbortFlush_MergesBackNewerWins`, `TestOverlay_StaleEntry_FollowsCommittedSize`, `TestVFS_OracleRandomOps`, `TestVFS_TruncateThenGrow_OldBytesDoNotResurface`, `TestWAL_Compact_KeepsOnlyLive` |
| I-36 | `TestCluster_OtherGatewaySeesPreviousVersionUntilApply`, `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes` |
| I-37 | `FuzzWALReplay`, `TestOverlay_CommitFlushed_PostRenameError_OffsetsValid`, `TestWAL_Compact_SyncDirFails_LogStillUsable`, `TestOverlay_OracleRandomOps`, `TestOverlay_RestartReplaysDirtyWALs`, `TestOverlay_StaleCompactTempRemoved`, `TestOverlay_TruncateUpDuringFlush_SurvivesReopen`, `TestWAL_AppendAfterFailedSync_NoResurrectedRecord`, `TestWAL_AppendReplayRoundTrip`, `TestWAL_Compact_KeepsOnlyLive`, `TestWAL_RecordSizeBoundaries`, `TestWAL_ReplayStopsAtTornTail` |
| I-63 | `TestWAL_CreateFsyncsDirectory` |
| I-78 | `TestFlusher_Buffers_BoundedByChunkWorkers`, `TestFlusher_ConflictRetry_ReassemblesFromWAL`, `TestFlusher_DoesNotFillCache` |
| I-169 | `TestFlusher_StragglerGrace_CutsWaitAfterKAcks`, `TestFlusher_StragglerGrace_Table`, `TestFlusher_StragglerGrace_NotBeforeKAcks` |
| I-171 | `TestClient_GetDeadline_FromContentLength` |
| I-168 | `TestWAL_GroupCommit_OneFsyncCoversConcurrentAppends`, `TestWAL_GroupCommit_FailedFsyncFailsEveryWaiter`, `TestWAL_GroupCommit_LateAppendWaitsForNextFsync`, `TestOverlay_PendingWrite_NotVisibleUntilFsynced`, `TestOverlay_FailedGroupFsync_DropsEveryPendingWrite`, `TestOverlay_CommitFlushed_WaitsForPendingAppend`, `TestOverlay_WriteDoesNotHoldInodeLockDuringFsync`, `TestOverlay_TruncateDuringPendingAppend_MatchesReplay` |
| I-203 | `TestReader_Readahead_DepthByBytes_Table`, `TestReader_Readahead_PrefetchIsBackgroundPriority`, `TestReader_Readahead_RandomReadCancelsUnjoinedPrefetch`, `TestReader_Readahead_JoinedPrefetchSurvivesCancel`, `TestFile_ReadAt_PrefetchStartsAfterTheRead` |
| I-166 | `TestReader_Readahead_SequentialFillsCache`, `TestReader_Readahead_RandomReadDoesNotPrefetch`, `TestReader_Readahead_BoundedInFlight`, `TestReader_Readahead_ForegroundJoinsPrefetch`, `TestReader_Readahead_SkipsHashZeroAndCached`, `TestReader_Readahead_FailureIsInvisible`, `TestPhaseHotPath_ConformingGatewayPasses`, `TestPhaseHotPath_NonConformingGatewayFails`, `TestPhaseHotPath_OneGatewayUnmovedCountersSkip` |
| I-79 | `TestFlusher_FullCluster_ErrorWrapsErrFull`, `TestOverlay_WALCap_WaitsForFlush`, `TestOverlay_WALCap_FailsAfterCapWait`, `TestOverlay_WALCap_FailsFastWhenFlushFailed`, `TestNFS_WriteAtCap_WaitsThenSucceeds`, `TestNFS_WriteWhenFull_IsNOSPC`, `TestVFS_ClusterFull_ENOSPC` |
| I-80 | `TestFlusher_BatchCommit_ResidualStaysDirty`, `TestOverlay_TotalDirty_CountsPlanBytes`, `TestOverlay_TotalDirty_FollowsAbortAndDrop`, `TestWAL_Compact_StreamsThroughBoundedBuffer`, `TestWAL_Compact_SyncDirFails_LogStillUsable`, `TestOverlay_CommitFlushed_PostRenameError_OffsetsValid` |
| I-83 | `TestFlusher_StragglerPut_CancelledAfterReplace`, `TestClient_PutTimeout_Table`, `TestNode_SlotBudget_Table`, `TestChunkRPC_PutCancelled_NoShardLeft` |
| I-98 | `TestReader_HedgesStalledHolder` |
| I-101 | `TestWAL_FsyncFailureReopens`, `TestWAL_CompactReopenFailureClosesLog` |
