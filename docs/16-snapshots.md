# 16 — Snapshots and previous versions

A snapshot is the whole volume as it was at one moment, kept readable while the live tree
moves on. A file's previous versions are its content at each snapshot. Both come from one
mechanism: a snapshot is a *name and a Raft index*, taken in O(1), and the FSM keeps the
pre-image of a change only while a snapshot still sees it. Nothing is copied when a
snapshot is taken; what it costs is the history it pins, released through the ordinary
tombstone and GC path when the last snapshot that needs it goes.

This builds on the trash (02 §Apply rules, 06 §1): a deleted entry is a *deleted root*
that keeps its inode and references until it is purged. Snapshots add to that the
pre-images of overwrites, truncates and renames, and a rule that keeps a deleted root in
the trash for as long as a snapshot reaches it.

*Status (2026-10-11): designed; docs, invariants, lookdev and stubs first. The numbers
below are stubs in `test/invariants/` until each PR lands its tests (PR 1 meta, PR 2
gateways and the leader's tick, PR 3 the web UI, PR 4 soak phase 19).*

## 1. Positions in time are Raft indices

Every applied command has a Raft index, identical on every node and strictly increasing,
and `Gen` already derives from it (01). Snapshots and history use the index, never a
clock: a snapshot's `At` (the FSM clock when it was taken) is for display only. So "the
volume as of snapshot `s`" is "every entry with index ≤ `s.Index`", a statement every
replica answers alike without a clock, and a rolling upgrade cannot shift it (§8).

## 2. Types and reads

```go
type SnapID uint32                       // from State.NextSnap; never reused

type Snapshot struct {
    ID    SnapID
    Name  string   // unique, 1..64 bytes, no "/", not "." or ".."; automatic: the UTC minute, "2026-10-11-1000Z"
    Index uint64   // Applied index of the Snapshot command: the volume as of every entry ≤ Index
    At    int64    // FSM clock when taken (display)
    Auto  bool     // taken by the leader's schedule (thinned, §4); false = manual (never thinned)
}

type NameSpan    struct { Parent Ino; Name string; From, To uint64 }   // the entry sat at Parent/Name for indices [From, To)
type FileVersion struct { Size uint64; Mtime int64; Version uint64; Chunks []Hash; From, To uint64 } // content for [From, To)

Inode: + NameIdx    uint64         // index at which the current (Parent, Name, live) span opened
       + DeletedIdx uint64         // index of the Trash that closed it; 0 while live (set with DeletedAt)
       + DataIdx    uint64         // index of the last Commit (files)
       + Names      []NameSpan     // closed, pinned spans, oldest first (copy-on-write, like Chunks)
       + Prev       []FileVersion  // closed, pinned contents, oldest first (copy-on-write)

State: + Snaps    map[SnapID]*Snapshot
       + NextSnap SnapID
       + Former   map[Ino]map[Ino]struct{} // derived: parent → inodes with a NameSpan under it
       + Hist     map[Ino]struct{}         // derived: inodes with Names or Prev (the reap's index)
```

`Former` and `Hist` are rebuilt on decode and in `Clone` exactly as `Trash` is (01). The
codec changes are in 01 §Binary codec (snapshot frame version 5, I-184).

**Definitions**, each a pure function of `State`, so every node computes the same:

- An index interval `[from, to)` is **pinned** when some snapshot has `from ≤ Index < to`.
- An inode's **current span** is `[NameIdx, DeletedIdx)` when `DeletedAt != 0`, else
  `[NameIdx, ∞)`. Deleted means `DeletedAt != 0`, never `DeletedIdx != 0`: a root deleted
  before the upgrade decodes with `DeletedIdx = 0` and must not come back to life in the
  first snapshot.
- `SpanAt(in, x)` is the `(Parent, Name)` the inode had at index `x`: the current span's
  when it covers `x`, else the one `NameSpan` in `Names` covering `x`, else none.
- An inode is **reachable at `x`** when `SpanAt(in, x)` is not none and its parent at `x`
  is reachable at `x`; the root always is. A subtree whose root is in the trash has
  children with open spans, so reachability, not `SpanAt`, is the visibility test for
  handles, `GetInodeAt` and the purge.
- A deleted root is **pinned** when some snapshot index makes it, or any inode under it,
  reachable. Quick reject: no snapshot with `Index < DeletedIdx` means unpinned without a
  walk, since every span under the root, and every outside span of an inode moved in,
  ends before the root's delete.
- **No history** is kept under an ephemeral name (`meta.IsEphemeral`, 06 §1) or the
  reserved folder `/.tailraid` (13 §5): a Commit, Rename or Trash there keeps nothing, and
  a snapshot never shows those names (the spirit of L-208).

**Reads as of a snapshot** (`meta.Store`, 02 §Reads), deterministic because a union is
filtered and then sorted by name:

- `ContentAt(in, x)`: the live `(Size, Mtime, Version, Chunks)` when `DataIdx ≤ x`, else the
  `FileVersion` covering `x`.
- `ReadDirAt(dir, s)`: the candidates are `Children(dir)` ∪ `Trash[dir]` ∪ `Former[dir]`;
  those whose `SpanAt(s.Index)` is `(dir, ·)` are kept and sorted by name. Names are unique
  at any index because every span opens from a live name table and a trashed root's span
  is closed (a rename `a → b` over a live `b` closes old `b` at the same index).
- `LookupAt(dir, name, s)`: `Children[name]` when its span at `s` is `(dir, name)`, else the
  one candidate in `Trash[dir] ∪ Former[dir]` whose span at `s` is `(dir, name)`.
- `ResolveAt(s, parts)` walks `LookupAt` from the root, so it is reachability by
  construction; `GetInodeAt(s, ino)` is the inode with `ContentAt` applied and `SpanAt` as
  `Parent` and `Name`, `ErrNotExist` unless reachable at `s.Index`; `PathAt(s, ino)`;
  `Snapshots()` sorted by `Index`; `VersionsOf(ino)`: the distinct contents (`Prev` plus
  the live one), each with the snapshots whose index its interval covers.

## 3. Commands and the span rule

Three commands, codec version 5 (02 §Codec versions; the leader refuses them with
`ErrCodec` while a live member reports an older codec, as it does for codec 4):

| Op | # | Payload | Result |
|---|---|---|---|
| `Snapshot` | 18 | `{Name, Auto, Now}` | `SnapID`; `ErrExist` for a taken name, `ErrBadCommand` for an invalid one |
| `SnapshotDelete` | 19 | `{ID, Now}` | `ErrNotExist` for an unknown ID; it removes the snapshot and nothing else |
| `SnapshotReap` | 20 | `{Limit, Now}` | `More bool`: walks `Hist` in `Ino` order and each inode's `Prev` and `Names` in `To` order, dropping up to `Limit` entries whose interval no snapshot pins; a dropped `FileVersion` applies `−1` per non-zero position, tombstoning at zero at the FSM clock, never underflowing; a dropped `NameSpan` leaves `Former[Parent]` when it was the inode's last span under that parent; an inode leaves `Hist` when both lists are empty |

`Commit` gains a result field, `Kept bool`: whether the pre-image became a version. The
FSM decides it, so a gateway never guesses against a concurrent snapshot.

**The span rule** (one paragraph for every apply rule it touches, 02): *every change of an
inode's `(Parent, Name, deleted)` at index `i` closes the current span at `i` and, iff the
closed span is pinned, appends it to `Names` (the inode joins `Former[Parent]` and `Hist`);
`Trash` sets `DeletedIdx = i` and opens nothing; `Rename`, `Restore` and folding open the
new span at `i` (`NameIdx = i`, `DeletedIdx = 0`).* So:

- **Rename** (within a directory too, and a rename-over whose destination goes to the
  trash by today's rule) records the old place when pinned.
- **Trash** keeps today's effect and sets `DeletedIdx`. **Restore** to the same or another
  place has nothing to keep (`[NameIdx, DeletedIdx)` is already closed) and opens the new
  span at its own index, so a snapshot taken in the gap sees nothing.
- **Folding** (a directory trashed while roots deleted from inside it earlier are in the
  trash, 02): today `fold` clears the nested root's `DeletedAt` and re-links it. Now the
  nested root's deleted span `[NameIdx, DeletedIdx)` is kept in `Names` when pinned and the
  re-link opens a new span at the folding index, so a snapshot between the two deletes
  does not see it, while "a tree removed entry by entry is one root" and a restore that
  brings everything back (I-152) are unchanged.
- **Commit** at `i`: the pre-image (`Size`, `Mtime`, `Version`, `Chunks`, interval
  `[DataIdx, i)`) is kept as a `FileVersion` iff some snapshot in that interval has
  `SpanAt(in, Index) ≠ none` (cheap, no chain walk; a version retained inside a trashed
  subtree is the one leak this allows and the purge closes it). When kept, **no reference
  is dropped**: the version holds them; otherwise the refs go as today, `−1` at rewritten
  indices and past the truncate point. `DataIdx = i` either way. `Refs` keeps its meaning,
  one per `(list, index)` position, where a list is a live `Chunks` or a retained
  `FileVersion.Chunks`; `HashZero` positions are never counted. Commit batches (L-191) are
  fine: a snapshot between batches sees the half-flushed file as a reader on another
  gateway does, and `Prev` gains one entry per pinned interval, so `len(Prev) ≤ len(Snaps)`
  for every file whatever the churn.
- **Unlink and Rmdir**, still hard deletes (ephemeral names, the codec-4 fallback), refuse
  a pinned non-ephemeral entry with `ErrPinned`; the mount's path is `Trash`.
- **Purge** (user or retention) skips a pinned root: a user's purge is `ErrPinned` naming
  the newest snapshot that sees it; the retention tick leaves it in the trash (the browser
  shows "kept by snapshot X") and takes it on the first tick after the last such snapshot
  goes. A purge subtracts one reference per non-zero position of every deleted file's live
  list **and** its `Prev`, removes each deleted inode from `Former[sp.Parent]` for every
  span it had, and for a deleted directory `D` drops every `NameSpan{Parent: D}` from the
  members of `Former[D]` and `Former[D]` itself (safe: `D` was reachable at no snapshot).
  `Versions` and `VersionBytes` follow.
- **SetAttr** (mode, owner, times) is not versioned: a snapshot shows an inode's current
  mode and owner with the size and mtime of the pinned content (§6).
- **Directories** have no data history: a listing as of a snapshot is derived from the
  children's spans (§2), so a 100k-entry directory costs nothing per change.

## 4. Schedule, thinning and reaping (the leader's tick, 06 §2)

```yaml
snapshots:
  every: 1h          # the leader takes an automatic snapshot every `every`; 0 = none (default 1h)
  keep:
    hourly: 24       # the newest automatic snapshot of each of the last N hour buckets (UTC)
    daily: 30
    weekly: 8        # 0 for any bucket keeps none of that kind
```

Every key is written to the file only when set (`omitempty`), so an older release reads
the file after a rollback (I-61). `every` under one minute and a `keep` count over 10 000
are refused by `Validate`. Each tick the leader, with the clamped clock `fsmNow =
max(leader clock, LastNow)` of 06 §2:

1. proposes `Snapshot{Auto: true, Name: <the UTC minute>}` when the newest automatic
   snapshot is older than `every`; an `ErrExist` on that name (a far-ahead `LastNow` clamps
   the clock to one minute) counts as "taken this minute", never a retry;
2. computes the keep set from the automatic snapshots' `At`: the newest automatic snapshot
   of each of the last `hourly` hour buckets, `daily` day buckets and `weekly` ISO-week
   buckets (UTC), and proposes `SnapshotDelete` for every automatic snapshot outside it,
   oldest first; a manual snapshot is never thinned;
3. proposes `SnapshotReap{Limit: 1024}` while the result says `More`, before the retention
   `Purge` and `GCDelete`: a reaped version tombstones the chunks that reach zero, which
   `GCDelete` collects after grace, the same path as a purge.

A leader whose members lack codec 5 skips all three (the `ErrCodec` rule); `check` warns
when the schedule is on and no automatic snapshot is younger than `2 × every` (12 §11).

## 5. Restore: revert, copy, tree

None of these is an FSM command.

- A **revert** of a file to a version is an ordinary `Commit{Ino, BaseVersion, HasTruncate:
  true, TruncateTo: 0, NewSize: v.Size, Writes: every index of the version, HashZero holes
  included}` built by the gateway from the version's hashes, with `NewChunks` empty (every
  hash is in the table because the version holds a reference), after the gateway has
  flushed or dropped the inode's dirty overlay (else a later `ErrConflict` rebase would lay
  dirty bytes over the reverted content). The replaced content is kept iff a snapshot
  pins it, like any overwrite, and `Kept` says which.
- A **copy** is `Create` plus that `Commit` under a new name beside the file (the browser
  proposes `<stem> (<snapshot>)<ext>`).
- A **tree restore** walks the snapshot's tree from a path and issues `Mkdir`, `Create` and
  `Commit` by hash under the target, so no shard moves or is uploaded: an existing
  directory is descended, an existing file or symlink is skipped and reported, and
  `.snapshots` and `.tailraid` are refused as target names at the root. The CLI
  (`snapshot restore`, 12 §11) and the browser (`restore-tree`, 13 §3) use the same
  `vfs.RestoreTree`.

## 6. What a snapshot does not keep

- **Attributes.** Mode, owner and times set by `SetAttr` are the inode's current values.
- **Ephemeral names and `/.tailraid`.** Never in history; a snapshot never shows them.
  A thumbnail of a pinned version is kept by the sweep while its `(ino, gen, version)`
  names a retained version (13 §5, I-160).
- **Dirty bytes.** A snapshot sees committed content only: bytes acknowledged into a
  gateway's WAL but not yet flushed are not in it, exactly as a reader on another gateway
  does not see them (04). "Take a snapshot" in the browser flushes the serving gateway
  first so the user's own last writes are in it; other gateways' dirty bytes are not.
- **Deleted roots purged before the first snapshot** that would have reached them: a
  purge is final.

## 7. Bounds

- A snapshot name is 1 to 64 bytes, no `/`, not `.` or `..`; an automatic name is the UTC
  minute, so one automatic snapshot a minute at most. `SnapID` is a `uint32`, never reused.
- `SnapshotReap.Limit` is at most 1024 per entry, as `Purge` and `GCDelete` (I-103).
- `len(Prev) ≤ len(Snaps)` for every file. The worst case is a continuously rewritten
  large file: one chunk list per snapshot, 8 MiB of hashes per TiB at 4 MiB chunks, 62
  snapshots under the default keep set ≈ 0.5 GiB on every node for a 1 TiB file; `check`
  warns when one file's `VersionBytes` exceeds a tenth of the volume's logical bytes
  (01 §Memory model).
- The NFS `fileid` of an entry in a snapshot is the bare `Ino`, as live (07): `du`, `cp`,
  `tar` and `rsync -H` fold only entries whose `nlink > 1`, and files report 1.

## 8. Rolling upgrade

A node upgraded at index 100 and one upgraded at 200 disagree on the `From` of a span
closed at 150 (the older binary never set `NameIdx`). Decisions agree at every snapshot
index (a snapshot can exist only once every member runs codec 5, so both nodes have set
every index a snapshot could fall after), so visibility is identical on every node;
`From` is informational, the reap orders by `(Ino, To)`, which a codec-5 binary always
set, and byte-identical frames across nodes (I-22) are promised for histories fully on
codec 5. A v4 state decodes with `NameIdx = DataIdx = 0`, exact ("before index 1").

## Invariants

- **I-179** — A snapshot is a name and an index: `Snapshot` records `(ID, Name, Index = its
  own index, At, Auto)` and changes nothing else; names are unique and valid, IDs are never
  reused; and `ReadDirAt`, `LookupAt`, `ResolveAt` and `GetInodeAt` for a snapshot return,
  on every node, exactly the names, inodes and contents the live reads returned for every
  entry up to `Index`, and nothing created, renamed into place or committed after it.
- **I-180** — Every change of an inode's `(Parent, Name, deleted)` at index `i` closes its
  current span at `i` and keeps it as a `NameSpan` iff a snapshot pins it (`Rename`,
  `Trash`, `Restore` and folding alike), and `Commit` keeps the pre-image as a
  `FileVersion` with its references iff a snapshot in `[DataIdx, i)` sees the file,
  dropping the references otherwise; nothing is kept under an ephemeral name or the
  reserved folder; `Unlink` and `Rmdir` of a pinned entry are `ErrPinned`; every retained
  entry's interval is pinned at every instant it exists and `len(Prev) ≤ len(Snaps)`
  (`CheckInvariants`); and the derived `Former` and `Hist` equal a recount.
- **I-181** — `SnapshotDelete` removes only the snapshot; `SnapshotReap` drops, walking
  `Hist` by `Ino` and each inode's lists by `To`, at most `Limit` per entry, exactly the
  retained entries no snapshot pins, applying `−1` per non-zero position of a dropped
  version, tombstoning at zero at the FSM clock, never underflowing; after it `Versions`,
  `VersionBytes`, `Former` and `Hist` equal a recount, the same on every replica.
- **I-186** — A revert is a `Commit` of the version's hashes (a truncate to zero plus every
  index, holes included) issued after the inode's dirty bytes are flushed or dropped, and
  reads back the version's bytes on every gateway; a copy is a new inode with the same
  content; a tree restore recreates the snapshot's names and contents under the target
  with no shard moved or uploaded, descends an existing directory, skips an existing file
  and reports every entry; the replaced content is kept iff a snapshot sees it, and
  `Commit`'s `Kept` says which.

## Tests

Planned names are written without backticks until the tests exist (`scripts/check-docs.sh`
checks every backticked name); each ID is cited by a stub in `test/invariants/` meanwhile.

| Invariant | Tests |
|---|---|
| I-179 | TestFSM_Snapshot_Table, TestFSM_Snapshot_NameBoundaries, TestStore_ReadsAt_Table, TestFSM_ReadDirAt_Determinism, TestCluster_SnapshotViewAcrossNodes, `TestModelCheck_Long` |
| I-180 | TestFSM_SpanRule_Table, TestFSM_Commit_PinnedKeepsVersion, TestFSM_Commit_UnpinnedDropsRefs, TestFSM_Fold_KeepsDeletedSpan, TestFSM_Ephemeral_NoHistory, TestFSM_UnlinkRmdir_PinnedRefused, `TestCheckInvariants_DetectsEachViolation`, `TestModelCheck_Long` |
| I-181 | TestFSM_SnapshotReap_Table, TestFSM_SnapshotReap_LimitBoundary, TestFSM_SnapshotDelete_OnlyTheSnapshot, TestGC_ReapThenCollect, `TestFSM_DeterministicAcrossReplicas`, `TestModelCheck_Long` |
| I-186 | TestVFS_Revert_Table, TestVFS_Revert_FlushesDirtyFirst, TestVFS_SaveCopy, TestVFS_RestoreTree_SkipsExisting, TestFSM_Commit_ResultKept, TestCluster_RevertReadsOnEveryGateway |
