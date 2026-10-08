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
| SETATTR mode/uid/gid/times | `Change(fs).Chmod/Chown/Chtimes` | — |
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
    sizeSet  bool     // a truncate happened since the last flush
    ext      extents
    wal      *WAL
    dirtyB   uint64
    lastW    time.Time
    flushing bool
}
```

Extent-set semantics (all O(log n + k) on the sorted slice):

- `insert(x)`: remove the parts of existing extents covered by `[x.off, x.off+x.n)`,
  splitting an extent into two when `x` lands inside it, then insert `x`. Later writes win
  byte-for-byte (last-writer-wins at byte granularity within one gateway).
- `overlapping(off, n)`: extents intersecting the range, in offset order.
- `truncate(size)`: drop extents at or beyond `size`, clip the one straddling it; set
  `size`, `sizeSet = true`. A truncate *up* only sets `size`; the gap reads as zeros.
- `Write` beyond the current size extends `size`; the hole between old size and `off` reads
  as zeros (no extent is created for it).

### 2.1 WAL record layout

One file per dirty inode at `<data_dir>/staging/<ino>.wal`, append-only:

```
record := crc32c u32 | type u8 | off u64 | len u32 | data[len]
type    := 1 = write (data follows), 2 = truncate (len = 0, off = new size)
```

`crc32c` covers `type|off|len|data`. All integers little-endian.

```go
type WAL struct { f *os.File; off int64; sync bool }
func openWAL(path string, sync bool) (*WAL, error)
func (w *WAL) AppendWrite(off uint64, p []byte) (walOff int64, err error)   // write + f.Sync()
func (w *WAL) AppendTruncate(size uint64) error
func (w *WAL) ReadAt(p []byte, walOff int64) error
func (w *WAL) Replay(fn func(typ uint8, off uint64, data []byte, walOff int64) error) error
func (w *WAL) Reset() error                                                 // ftruncate(0) + fsync
```

Rules:

- `AppendWrite` returns only after `f.Sync()` succeeds. `sync` is always true in production;
  the field exists so the oracle tests can run fast, and no production code path sets it to
  false (no env var, no flag).
- `Replay` reads records in order and **stops at the first record whose CRC fails or whose
  length runs past EOF** (the torn tail from a crash mid-append). Everything before it is
  replayed into the extent set; the file is then truncated to that point.
- `Reset` happens only when the extent set is empty after a successful flush.
- `OpenOverlay` replays every `staging/*.wal` at startup, so a gateway that crashed with
  dirty data comes back with the same overlay and flushes it.

Per-WRITE cost: one `pwrite`, one `fsync`, one sorted insert. Nothing is held in memory but
extents.

## 3. Flush triggers

| Trigger | Default | Action |
|---|---|---|
| inode idle (no WRITE for) | 2 s | flush that inode |
| inode dirty bytes ≥ `FileDirtyHi` | 32 MiB | flush its fully covered chunks now |
| total dirty bytes ≥ `TotalDirtyHi` | 256 MiB | flush the largest inodes until below |
| total dirty bytes ≥ `MaxDirty` | 1 GiB | WRITE returns `NFS3ERR_IO` until it drains |
| shutdown | — | `FlushAll` with a bounded deadline, then exit (WAL survives anyway) |

Flush workers run per inode from a bounded pool (default 4) so a big copy does not starve
small files.

## 4. `FlushInode` — the exact procedure

```go
func (f *Flusher) FlushInode(ctx context.Context, ino meta.Ino) error
```

1. **Snapshot the plan.** `plan := ov.snapshotForFlush(ino)` atomically takes the current
   extents, `size`, `sizeSet` and installs a fresh empty extent set. New WRITEs continue to
   append to the same WAL and land in the fresh set. The WAL is not reset here.
2. **Base inode.** `base, ok := st.GetInode(ino)`. If `!ok` the file was unlinked (or a
   rename replaced it) → `ov.Drop(ino)`, return nil. Nothing is committed for a dead inode.
3. **Touched chunk set.** `CS := Vol.ChunkSize`. Touched = chunks intersecting any dirty
   extent ∪ `{ceil(plan.size/CS) − 1}` when `plan.size < base.Size` and `plan.size % CS != 0`
   (the new tail chunk must be rewritten because the FSM cannot trim bytes; it rejects such
   a Commit with `ErrNeedTailRewrite`).
4. **Assemble each touched chunk.** Effective length `eff = min(CS, plan.size − i·CS)`. If
   dirty extents fully cover `[i·CS, i·CS+eff)` read them from the WAL; otherwise read the
   base chunk (`Reader.ReadChunk(base.Chunks[i])`, zeros when `HashZero` or `i ≥
   len(base.Chunks)`) and overlay the WAL extents. Clip to `eff`.
5. **Holes.** If the assembled chunk is all zero → `ChunkWrite{i, HashZero}`; no upload.
6. **Hash and dedup.** `h = sha256(chunk)`. If `st.GetChunk(h)` exists (live or tombstoned)
   → `ChunkWrite{i, h}` with no upload; the Commit revives a tombstone.
7. **Policy.** `(k, m) = len(chunk) ≤ SmallChunk ? (1, Vol.M) : (Vol.K, Vol.M)`.
8. **Place.** `placement = Place(h, liveCandidates(peers.Live(), nodes), k+m)` where
   candidates exclude `Draining`/`Removed` nodes and nodes over capacity.
9. **Encode and PUT.** `shards = codec.Encode(chunk)`; PUT every slot in parallel (self via
   `local.Put`); always `cache.Put(h, chunk)`. Wait until **≥ k acks AND (all PUTs finished
   OR 10 s elapsed)**. Slots that failed are re-placed once (excluding failed nodes) and
   PUT again. Slots still missing are recorded as `0` (degraded). If acks < k → return an
   error; the plan stays dirty and is retried with backoff (1 s doubling to 60 s).
10. **Commit.** `Propose(Commit{Ino, BaseVersion: base.Version, NewSize: plan.size,
    Writes, NewChunks, Now})`.
    - `ErrConflict`: another gateway committed first. Reload `base`, recompute steps 3–9 for
      the same plan (dedup makes untouched chunks free), propose again. Dirty extents are
      never dropped on conflict.
    - `ErrUnknownChunk`: a dedup'd chunk was GC'd between step 6 and commit. Upload it (steps
      7–9 for that chunk) and propose again.
    - `ErrNotExist`: inode gone → `ov.Drop(ino)`, return nil.
    - `ErrNeedTailRewrite`: programming error in step 3; log at error level and retry with
      the tail chunk included.
11. **Wait and clear.** `st.WaitApplied(index)`; `ov.commitFlushed(ino, plan)` removes the
    plan's extents from the file's bookkeeping; if no extents remain, `wal.Reset()`.

Step 9's "≥ k acks" is the user's chosen degraded-write policy (see 05 §5). Full
redundancy is restored by the repairer (06).

## 5. Read path

```go
func (r *Reader) ReadChunk(ctx context.Context, h meta.Hash) ([]byte, error)
func (r *Reader) ReadAt(ctx context.Context, ino meta.Inode, p []byte, off int64) (int, error)
```

1. `HashZero` → caller zero-fills. Cache hit (`cache.Get` verifies sha256) → return.
2. `c := st.GetChunk(h)`; missing → `EIO` (metadata says the chunk exists; this is a bug or
   a lost majority, not a normal path).
3. `order = peers.Rank(nonZero(c.Placement))`: self first, then by RTT, S3 nodes last.
4. Fetch the first `k` shards in parallel. On any failure or `ErrCorrupt`, pull the next slot
   in `order` until `k` good shards or the list is exhausted (→ `EIO`, logged with the chunk
   hash and the slots tried).
5. `codec.Decode(shards, c.Size)`; verify `sha256 == h`. Mismatch → `EIO`; the scrubber
   finds the bad shard (06 §6).
6. `cache.Put(h, chunk)`.

`ReadAt` walks chunk indexes, copies `min(eff, c.Size)` bytes from each chunk and zero-fills
the remainder (a short dedup'd chunk under a truncate-up). The overlay is consulted first:
`Overlay.Read` merges WAL extents over base bytes.

## 6. Visibility

- **Issuing gateway:** read-your-writes immediately after the WRITE reply, from the overlay.
  `GETATTR` size comes from the overlay while dirty.
- **Other gateways:** see the previous committed version until their FSM applies the Commit
  (milliseconds after Raft commit). They never see a torn mixture of old and new chunks,
  because the chunk list is replaced atomically by one Commit.
- **Two gateways writing the same file:** disjoint chunks → both Commits land (the second
  rebases); same chunk → last Commit wins for that whole chunk. This is documented, not
  hidden.

## 7. When the cluster is unavailable

| State | WRITE RPC | Flush |
|---|---|---|
| Raft has a leader, ≥ k live storage nodes | OK | proceeds |
| Raft has a leader, < k live | OK until `MaxDirty`, then `NFS3ERR_IO` | fails at step 9, retried with backoff |
| no leader / `Health == Stale` | `NFS3ERR_ROFS` for new writes; already-dirty data keeps retrying | fails at step 10, retried |
| gateway process restart | — | WAL replayed, flush resumes |

The WAL is the only place acknowledged-but-unflushed bytes live. It is fsynced per record
and replayed on start, so neither a crash nor a long outage loses it.

## 8. Durability contract (normative, quoted verbatim in README and 05)

"No data loss" means every byte whose WRITE was acknowledged and every metadata op that
returned OK is returned unchanged by any later READ on any live gateway, under these failures:
(a) permanent loss of any **m** nodes' disks; (b) gateway process crash or power loss at any
instant, including right after an ack (WAL replay); (c) leader crash or partition at any
point in Apply; (d) corruption of up to m shards per stripe (detected by hash, repaired);
(e) kill -9 at any fsync step of a shard write (temp files swept, never served); (f) loss of
a Raft minority. **Not covered:** > m nodes lost before repair; permanent loss of Raft
majority; storage that lies about fsync; > m corrupt shards in one stripe; destruction of
the gateway's own disk within the ≤ 2 s WAL→flush window (option for later: WAL mirroring
to one peer before ack). Degraded writes (user's choice) hold < m parity until repair.

### 8.1 fsync table

| Component | What is fsynced | When |
|---|---|---|
| WAL (`staging/<ino>.wal`) | the file | after every appended record, before the WRITE reply |
| shard store (`shards/hh/<key>`) | temp file, then rename | `write → f.Sync() → Close → Rename`; no directory fsync (a lost rename is found by the repairer) |
| raft log/stable (`raft/raft.db`) | bbolt | `NoSync = false` (every Raft append) |
| snapshots | `FileSnapshotStore` | fsync on `Close` |
| snapshot of FSM state | none beyond above | FSM is in memory; it is rebuilt from log + snapshot |

**No production path skips fsync and there is no test-mode variable.** Tests that need
speed use `raft.NewInmemStore`, `sysfs.Fake` and `WAL{sync: false}` constructed only from
`_test.go` files; durability tests use real temp directories.

### 8.2 Future option: WAL mirroring

Closing the "gateway disk destroyed within 2 s" gap would mean sending each WAL record to
one live peer (`POST /v1/wal/<ino>`) before the WRITE reply, and replaying mirrored WALs
on the peer when the gateway does not come back. Cost: one tailnet round trip per WRITE.
It is deliberately not in the barebones scope; the seam is `WAL.AppendWrite`.

## Invariants

- **I-7** — A WRITE is acknowledged only after its bytes are fsynced to the WAL, and a Commit is proposed only after ≥ k shards of every new chunk are durable on distinct nodes; the Commit becomes visible only after Raft commit.
- **I-11** — A shard file visible under its final name has content matching its header hash; temp files are never served.
- **I-34** — The issuing gateway returns its own acknowledged bytes on any subsequent READ (read-your-writes), before and after flush.
- **I-35** — A flush never drops dirty extents: on `ErrConflict`, `ErrUnknownChunk` or PUT failure the same plan is retried; extents are removed only after `WaitApplied` of the Commit that carries them.
- **I-36** — Other gateways observe either the previous committed chunk list or the new one, never a mixture.
- **I-37** — WAL replay after a crash restores exactly the records whose CRC verifies, in order, and discards the torn tail; replayed bytes equal the acknowledged bytes.

## Tests

| Invariant | Tests |
|---|---|
| I-7 | `TestVFS_CloseFlushIsDurable`, `TestVFS_PerRPCOpenCloseSequence`, `TestFlusher_RequiresKAcks`, `TestFlusher_DegradedCommitRecordsZeroSlots`, `TestFlusher_BelowKLeavesWALIntact`, `TestCrash_AtEveryPoint`, `TestCluster_WriteReadAllNodes` |
| I-11 | `TestStore_PutGetRoundTrip`, `TestStore_CrashBeforeRename_LeavesNoVisibleShard`, `TestStore_TornWrite_DetectedOnRead`, `TestStore_GetVerifiesHash_CorruptedBitFlip` |
| I-34 | `TestVFS_OracleRandomOps`, `TestVFS_WriteAtNonZeroOffset_PreservesPrefix`, `TestNFS_WriteSequence_MultiRPC` |
| I-35 | `TestFlusher_ConflictRebaseKeepsExtents`, `TestFlusher_UnknownChunkReupload`, `TestFlusher_PutFailureRetriesSamePlan` |
| I-36 | `TestCluster_ReaderSeesWholeVersions`, `TestE2E_ThreeNodes_NFSWriteOnA_ReadOnB_KillC` |
| I-37 | `TestWAL_ReplayTornTail`, `TestOverlay_RestartWithDirtyBuffer` |
