# 16 — Snapshots on mirror disks

A snapshot is the whole volume as it was at one moment. Snapshots live on **mirrors**:
dedicated disks, attached to a device of the cluster, that are never placement targets
and may be offline most of the time (a USB disk plugged in now and then, a disk in a
machine that sleeps). Each mirror is an independent, self-describing copy of the history
it captured: whole chunks stored once under their hash, and directory trees stored once
under theirs, so every snapshot after the first costs only what changed. Any one mirror,
alone, restores everything it holds, with or without the cluster.

The live pool keeps no history. A snapshot is copied out while it is taken, the pool
holds back garbage collection only for the length of that copy (§4), and from then on
the mirror owns it. Previous versions of a file are its content in the snapshots the
online mirrors hold.

*Status (2026-10-12): designed, docs, invariants, lookdev and stubs first; PR 1 built (the mirror row, the catalogue, ops 19–21, the sync hold, codec 7, and `internal/mirror`: the format, the differential writer, sweep, reconcile and the standalone export); PR 2a built (the host's sync worker, schedule and reconcile, the leader's hold_max, the config, `mirror` and `snapshot ls|create`); PR 2b built (`/.snapshots` on NFS and SMB, the 24-byte handles, the mirror RPC routes); PR 2c built (revert, copy, tree restore and versions in the vfs, `snapshot restore`, `versions`); the gaps built (`mirror add --adopt`, `SnapshotImport`, `open_mirror`, the hook's host ID, the CLI and schedule tests). The numbers
below are stubs in `test/invariants/` until each PR lands its tests (PR 1 meta and the
mirror store, PR 2a sync and the tick, PR 2b the gateways, PR 2c versions and restore, PR 3 the web UI, PR 4 soak
phase 19).*

## 1. Mirrors

A mirror is a `Node` row of `Kind == KindMirror` (4) with its `Host`, `ID` (16 random hex
characters), `Name` (`<host>/m<n>`) and `State`, like a disk row (14 §1), and like a disk
row it is never a Raft member. Unlike a disk it is **never a placement target**: placement,
the automatic policy, repair, the budget and the host cap do not see it (14 §9's rule that
only disks are targets is unchanged). A host may carry any number of mirrors; a cluster
may have none, and then nothing in this document runs.

```yaml
node:
  mirrors:
    - path: /Volumes/Archive/tailraid   # one entry per mirror on this host
```

`tailraid mirror add <path>`, run on the host, refuses a path that holds anything other
than an empty directory or a mirror of this cluster, writes the marker, appends the entry
to the config and proposes `NodeUpsert{Kind: KindMirror, Host: self}` through the leader
(the member op `add_mirror`). A path that already carries a marker of this cluster
re-homes its row to this host instead (a USB disk moved from the laptop to the desktop
keeps its history and its name); a marker of another cluster is refused unless `--adopt`
is given (§7); a disk removed and added back takes its old name again unless another
mirror took it meanwhile. The running node never rereads its config, so `mirror add` then asks it to
open the path (the admin op `open_mirror`, after rewriting a systemd unit as `node
add-disk` does, 14 §2) and the node runs the mirror's tick at once: its snapshots are
listed and the first one begins without waiting for the next minute. `mirror remove <name>` takes the row and its catalogue rows out of the
table and leaves the disk as it is.

**Online** means the host is live and the marker at the configured path names this
cluster and this mirror; anything else (unplugged, unmounted, host asleep or dead) is
**offline**. The host checks the marker on every maintenance tick (each minute) and
reports its mirrors in its `status` (online, used, free, newest snapshot). Only the host
can tell whether its disk is attached, so the host drives its mirrors (§4); nothing else
waits for a mirror, and an offline mirror costs the cluster nothing.

## 2. The mirror format (self-describing)

```
<path>/mirror_id                 JSON: {format: 1, cluster_id, mirror_id, host_id, created}
<path>/objects/<hh>/<hash>       a chunk: "TRS1" header (01 §Shard file format) + the chunk's logical bytes;
                                 <hash> is the volume's chunk hash, SHA-256 of those bytes
<path>/trees/<hh>/<hash>         a directory: the canonical tree encoding below; <hash> = SHA-256 of it
<path>/snaps/<id>.json           a snapshot's manifest, written last (§3)
<path>/tmp/                      temps; removed when the mirror is opened
```

A **tree** is one directory of one snapshot:

```
4 bytes  magic "TRT1"
uvarint  count, then each entry sorted bytewise by name:
         name string, type u8 (file 1, dir 2, symlink 3), mode u32, uid u32, gid u32,
         mtime i64, ino u64, gen u32,
         file:    size u64, uvarint n, n chunk hashes (HashZero for a hole)
         dir:     the child tree's hash
         symlink: target string
```

The encoding is canonical, so a directory whose entries did not change between two
snapshots has the same hash and is stored once; a change rewrites only the trees on the
path from the changed entry up to the root. Data and metadata are therefore both
**differential**: a snapshot writes the objects the mirror lacks, the trees
of the directories on the paths to what changed, and its manifest.

A **manifest** is `{format, cluster_id, id, name, index, at, auto, root, chunk_size, files,
dirs, bytes, new_bytes, missing: [hash…]}`: the snapshot's catalogue fields (§5), its root
tree, its logical size, what this snapshot added to the mirror, and the hashes of any chunk
the sync could not read (§3).

Every file is written as the shard store writes a shard (01): a temp in `tmp/`, write,
fsync, close, rename into place, fsync of the directory. An object is written before any
tree that names it, a tree before its parent, and the manifest after its root, so a
manifest on the disk names a snapshot whose every tree and every object (but the listed
missing ones) is on the disk, whatever crashed or was unplugged on the way. Objects carry
the TRS1 header, so a read verifies the payload against both the header and the name.

Self-describing means `tailraid mirror ls|cat|export <path>` read a mirror with no cluster,
no Raft and no Tailscale: list its snapshots, print a file of one, or copy a snapshot's
tree (or a part of it) out to a local directory with the original modes and times. It is
the recovery path when the cluster itself is gone.

## 3. Taking a snapshot (a sync)

1. `SnapshotBegin{Mirror, Name, Auto, Now}` is proposed by its host's schedule (§4) or by
   a user (`snapshot create`, the browser's "Take a snapshot"). The FSM records a
   catalogue row `{ID, Mirror, Name, Index: its own index, At: now, Auto, State: syncing}`.
   The mirror must be an active mirror row (`ErrNoSuchNode`), the name valid and unique on
   that mirror (`ErrBadCommand`, `ErrExist`), and the mirror must have no other syncing
   row (`ErrExist`). Whether the mirror is online is not the FSM's business: the proposer
   checks it, and a sync for a mirror that went offline meanwhile simply fails (step 5).
2. The mirror's host captures the volume **as of `Index`**: when its FSM applies that entry
   it clones the state there, as `FSM.Snapshot` does (01 §Memory model: the chunk lists
   are shared, about a quarter of a second per million inodes), and hands the clone to its
   sync worker. Every other node ignores the entry but for the hold of §4. The snapshot is
   the tree reachable from the root in that clone, committed content only (a gateway's
   dirty bytes are not in it, as another gateway does not read them, 04), without
   `/.tailraid` (the thumbnails, the stats history and the search index, 13, 17, 18) and
   without the ephemeral names of 06 §1. The trash is not in it.
3. The worker walks the clone depth first. For every chunk hash of every file the mirror
   does not hold (an in-memory set of the mirror's object hashes, loaded when the mirror is
   opened and kept current), it reads the chunk through the ordinary reader, which
   verifies it against its hash, marked background (06 §4, I-175) and metered by
   `mirrors.rate`, and writes the object. It then writes every tree the mirror lacks,
   children first, and the manifest last.
4. A chunk the reader cannot decode (fewer than `k` live slots) is not a failure: its hash
   stays in the tree, it goes into the manifest's `missing` list and the row ends
   `incomplete`; the next sync to that mirror tries it again, since it still lacks the
   object. Every other byte of the snapshot is exact.
5. The worker proposes `SnapshotEnd{ID, OK, Root, Files, Bytes, NewBytes, Missing, Now}`:
   with `OK` the row becomes `complete` (or `incomplete` with the count), and its root,
   sizes and missing count are recorded. Without `OK` (the mirror went offline, the disk
   filled, the host restarted and lost its clone, the leader ended a stale sync) the row is
   removed. A failed sync writes no manifest; the objects and trees it already wrote stay
   and make the next attempt shorter, which is how a first sync of a large volume that
   cannot finish in one go still converges.

A sync never changes the volume. Its cost to the pool is reads (at background priority and
`mirrors.rate`) and the hold.

## 4. The hold, the schedule and thinning

**The hold.** A chunk that the snapshot references may lose its last reference while the
copy runs (an overwrite, a purge). While any catalogue row is `syncing`, `GCDelete` collects
only chunks tombstoned **before the oldest syncing row's `At`**: the FSM applies
`Before = min(cmd.Before, min At of the syncing rows)` itself, so every replica deletes the
same entries (I-43), and every chunk referenced at `Index` stays collectable-later rather
than collected until the row ends. Nothing else holds anything: the trash, the purge and
the sweep are unchanged, since a purge only tombstones and the sweep only removes shards
the chunk table does not place. The leader ends a sync whose row is older than
`mirrors.hold_max` (24 h unless set) with `SnapshotEnd{OK: false}`, so a stalled copy never
holds GC for longer than that.

**The schedule** runs on each mirror's host, on its maintenance tick (each minute), for
the host's own mirrors; the leader's one part is ending a sync past `hold_max`, above,
since a dead host cannot end its own:

- for every online mirror with no syncing row, `SnapshotBegin{Auto: true, Name: <the UTC
  minute, 2026-10-12-0900Z>}` when its newest automatic snapshot is older than
  `mirrors.every` (1 h unless set; `0` takes none on its own);
- a mirror that has just come online after an offline stretch longer than `every` is due
  at once on the first tick: the catch-up;
- a syncing row of the host's mirror that the host is not copying (a restart dropped the
  clone the begin handed it) is ended as failed on the next tick, so it never holds GC
  until `hold_max`;
- an offline mirror gets nothing: no row, no hold, nothing kept in the pool for it. The
  snapshots it missed are simply not taken (the trash still keeps deletes for
  `trash.retention`);
- an `ErrExist` on the automatic name (a far-ahead `LastNow` clamps the clock to one
  minute) counts as taken this minute, never a retry;
- while any live member runs a codec older than 7 nothing is proposed (02 §Codec versions).

**Thinning**, per mirror, on the same tick: of the mirror's automatic snapshots the host
keeps the newest of each of the last `keep.hourly` hour, `keep.daily` day, `keep.weekly`
ISO week and `keep.monthly` month buckets (UTC; 24, 30, 8 and 12 unless set) and proposes
`SnapshotDelete` for the rest, oldest first. A manual snapshot is never thinned. When the
mirror's free space falls under its reserve (`max(5 %, 4 GiB)` of its filesystem, measured
by the host) the oldest automatic snapshot goes first, then syncs to that mirror stop with
a warning in `status` and `check` until space is freed; a manual snapshot is never deleted
for space.

**Nobody removes a snapshot by hand.** Thinning and the free-space reserve, both on the
host's tick, are the only ways a snapshot ever leaves a mirror: there is no CLI verb,
admin op or browser route that proposes `SnapshotDelete`, and the host's mirror tick is its only
proposer. A manual snapshot is therefore permanent: it is never thinned, never deleted for
space, and outlives every automatic one on its mirror. A mirror whose free space is taken
by manual snapshots stops syncing with a warning; the remedies are another mirror, or a
larger disk (copy the mirror's directory over and `mirror add` the new path, which
re-homes it), never a deletion.

```yaml
mirrors:
  every: 1h                # an automatic snapshot of every online mirror this often; 0 = none
  keep: {hourly: 24, daily: 30, weekly: 8, monthly: 12}
  rate: 50M                # bytes a second a sync reads from the pool; 0 = no limit
  hold_max: 24h            # a sync older than this is ended as failed
  stale_after: 7d          # check warns when a mirror has been offline this long
```

Every key is written to the file only when set (I-61). `Validate` refuses an `every`
under a minute, a `keep` count over 10 000 and a `hold_max` under an hour.

## 5. The catalogue, deletion and reconciliation

Raft holds the **catalogue**, not the snapshots: `State.Snaps`, one row per snapshot,
`{ID SnapID, Mirror NodeIdx, Name, Index, At, Auto, State (syncing | complete | incomplete | deleting),
Root Hash, Files, Bytes, NewBytes, Missing}`, about 150 bytes each. It is what lets every
gateway list every snapshot of every mirror, online or not, and say where a snapshot is
when its mirror is away.

Deletion is two steps, so that nothing leaves a disk without an explicit decision of the
schedule. `SnapshotDelete{ID, Done: false}`, proposed by the host's tick only, marks a
complete or incomplete row `Deleting`: it disappears from every listing at once. The
mirror's host, whenever the mirror is online, removes the manifest of every `Deleting`
row of its mirror, then runs a mark-and-sweep that removes every tree and object no
remaining manifest reaches (a syncing row's trees and objects are marked from the
worker's own set, so the sweep never races a sync), and then proposes
`SnapshotDelete{ID, Done: true}`, which removes the row. An offline mirror keeps its
`Deleting` rows until it returns.

A manifest on the disk with **no** row at all is never lost: it is a snapshot the
catalogue forgot (the mirror was removed with `mirror remove` and added back, the disk was
copied to a larger one, or `--adopt` took over another cluster's disk, §7), and the host
imports it on the mirror's tick with `SnapshotImport{Mirror, After, Name, At, Auto, Root,
Files, Bytes, NewBytes, Missing, Now}` (op 22). The FSM gives the row a fresh ID above both
`NextSnap` and `After`, the largest manifest ID on the disk, so neither the import nor any
later sync can name a manifest already there, and records it `complete` (or `incomplete`)
with the manifest's own name, time, auto flag, root and sizes; an imported automatic
snapshot is thinned by this cluster's schedule like any other. The host then writes the
manifest under the new ID and removes the old file. A crash between the two leaves the old
file beside a row of the same name and root: the next tick finishes the rename, or removes
the old file when the new one is already there, the one manifest ever removed without a
`Deleting` row, since it is a copy. A rowless manifest whose name another row of the
mirror holds with another root is imported as `<name>.<old id>`. A sync never replaces a
manifest on the disk: it fails instead, and the next one takes a higher ID. So no sequence of `mirror remove`, `mirror add` and reconciliation
removes a snapshot that thinning did not choose. The host never touches a disk whose
marker names another cluster until `mirror add --adopt` rewrites the marker.

## 6. Reading, versions and restore

**On the mounts.** `/.snapshots` is a directory the vfs synthesizes at the root (07): it is
absent from the root's listing, resolves by name, and holds one directory per mirror
(`<host>-m<n>`), each holding one directory per `complete` or `incomplete` snapshot by
name. Inside, the tree is read from the mirror's trees; a file's bytes come **from the pool
when the chunk is still there** (the chunk table holds it with `k` live slots: the reader
reads it as any chunk, cache and readahead included) and otherwise from the mirror's
object, fetched from its host over the node RPC (`GET /v1/mirrors/{id}/objects/{hash}`,
`/trees/{hash}`, authorized as the shard routes are, 08) and verified against its hash.
Every mutation under `/.snapshots` is `EROFS`. An offline mirror's directory still lists
its snapshots (the catalogue), and reading inside one fails with `EIO`; the browser says
why.

**Versions of a file** are computed on demand: for each online mirror, the path is
resolved in each of its snapshots' trees (trees are immutable, so a gateway caches them
by hash), and snapshots whose entry carries the same chunk list are grouped as one
version, newest first. The live content is the `current` version, marked with the
snapshots that hold it or "in no snapshot yet". Versions follow the path, not the inode: a
renamed file's earlier names are found by browsing the snapshot. An offline mirror
contributes "n snapshots on `pike-m1`, offline" and nothing else.

**Restore** is a write into the live volume, never a change to a mirror:

- a **revert** makes a version the file's current content: the gateway flushes or drops
  the file's dirty bytes, then proposes one `Commit{HasTruncate: true, TruncateTo: 0,
  NewSize, Writes: every index}`, where a chunk the pool still holds is committed by its
  hash (no upload, the dedup path of 04 §4) and any other is read from the mirror,
  verified and uploaded as a new chunk first;
- a **copy** is a `Create` beside the file plus that `Commit`;
- a **tree restore** recreates a snapshot's directory under a target in the live volume
  the same way, entry by entry: an existing directory is descended, an existing file or
  symlink is skipped and reported, `.snapshots` and `.tailraid` are refused as names at
  the root.

The CLI (`tailraid snapshot restore`, `tailraid versions --revert|--copy`, 12 §11) and the
browser (13 §4) use the same `vfs` calls.

## 7. Disaster recovery

A mirror is a copy of the volume's history that needs nothing from the cluster: its trees
carry names, modes, owners and times, and its objects the bytes. When a cluster is lost
beyond `recover` (05 (h), 12 §9), a new cluster adopts the mirror with `tailraid mirror
add <path> --adopt`: the marker is rewritten to the new cluster and this host (its mirror
ID and so its history stay), the manifests become catalogue rows on the host's next tick
(§5), and `tailraid snapshot restore <mirror>/<name> /` puts the volume back, uploading
every object the new pool lacks. Without any cluster, `tailraid mirror export <path>
<snapshot> <dir>` copies the files out.

A mirror holds the volume's bytes in the clear, like every disk of the volume (encryption
at rest is out of scope, 08): whoever holds the USB disk holds the files.

## 8. Bounds and gaps

- One sync per mirror at a time; any number of mirrors sync at once, each at
  `mirrors.rate`.
- A snapshot name is 1 to 64 bytes, no `/`, not `.` or `..`, unique per mirror; an
  automatic name is the UTC minute. `SnapID` is a `uint32`, never reused.
- A file's version list reads one tree path per snapshot: for 62 snapshots of a path ten
  levels deep, at most 620 tree reads, each cached by hash.
- Not kept: a gateway's dirty bytes, `/.tailraid`, ephemeral names, the trash, xattrs (none
  exist), hard links (none exist). Mode, owner and times are kept per snapshot.
- Not provided: the Windows Previous Versions tab (15 §8), a writable clone, a snapshot of
  one folder, a diff between snapshots, encryption of a mirror.

## Invariants

- **I-179** — A snapshot is the volume as of one index: `SnapshotBegin` records its row
  with `Index` its own index and changes nothing else in the volume, a mirror has at most
  one syncing row, names are unique per mirror and IDs are never reused; the mirror's
  host captures the state at exactly that index, and the snapshot's trees and objects
  reproduce every file, directory and symlink reachable from the root then, with its
  name, mode, owner, times and committed bytes, and nothing under `/.tailraid`, no
  ephemeral name and nothing in the trash.
- **I-180** — A mirror is self-describing, differential and crash-safe: an object is the
  chunk's bytes under its hash and a tree the canonical encoding under its hash, each
  verified on read; a sync writes only the objects and trees the mirror lacks; every
  object is durable before a tree names it, every tree before its parent and the
  manifest last (temp, fsync, rename, directory fsync), so a manifest on the disk names a
  snapshot whose every byte is on the disk but the chunks it lists as missing; temps are
  removed at open; and `tailraid mirror ls|cat|export` read it with no cluster.
- **I-181** — While a catalogue row is syncing, `GCDelete` collects no chunk tombstoned at
  or after the oldest syncing row's `At`, the same on every replica, so every chunk the
  snapshot references stays readable until `SnapshotEnd`; a sync ends `complete`,
  `incomplete` with its missing chunks listed, or failed with its row removed and no
  manifest written; and the leader ends as failed a sync older than `mirrors.hold_max`.
- **I-183** — A snapshot leaves a mirror only by its host's thinning or free-space
  rule: no CLI verb, admin op or web route proposes `SnapshotDelete`, and a manual
  snapshot is never deleted; `SnapshotDelete{Done: false}` marks a row `Deleting` and
  nothing else, and the mirror's host, while the mirror is online, removes exactly the
  manifests of its `Deleting` rows, then every tree and object no remaining manifest or
  running sync reaches and never one that a remaining manifest reaches, then removes the
  rows with `Done: true`; a manifest with no row is imported by `SnapshotImport` under an
  ID above every manifest on the disk and removed only as the duplicate of the row it
  became; a sync never replaces a manifest; and a disk whose marker names another cluster
  is never changed except by `mirror add --adopt`.
- **I-186** — A revert, a copy or a tree restore from a snapshot writes into the live
  volume exactly the snapshot's bytes, committing by hash every chunk the pool still holds
  and uploading every other from the mirror after verifying it; a revert flushes or drops
  the file's dirty bytes first; a tree restore descends an existing directory, skips an
  existing file and reports every entry; nothing on a mirror changes.

## Tests

Planned names are written without backticks until the tests exist; each ID is cited by a
stub in `test/invariants/` meanwhile.

| Invariant | Tests |
|---|---|
| I-179 | `TestFSM_SnapshotBegin_Table`, `TestFSM_SnapshotName_Boundaries`, `TestMirror_SyncCapturesIndex`, `TestMirror_SyncExcludesReservedEphemeralTrash`; planned: TestCluster_SnapshotReadsAsOfIndexOnEveryGateway, and the model checker's snapshot steps |
| I-180 | `TestMirror_TreeEncoding_Canonical`, `TestMirror_SecondSyncWritesOnlyChanges`, `TestMirror_CrashAtEveryWrite_ManifestImpliesComplete`, `TestMirror_ObjectVerifiedOnRead`, `TestMirror_OpenSweepsTemps`, `TestMirror_ExportAndCat_NoCluster`, `TestCLI_MirrorStandalone_LsCatExport` |
| I-181 | `TestFSM_GCDelete_HeldBySyncingRow`, `TestFSM_SnapshotEnd_Table`, `TestMirror_UnreadableChunk_Incomplete_RetriedNext`, `TestFSM_DeterministicAcrossReplicas`, `TestSchedule_LostAndExpiredSyncsEnded`; planned: TestCluster_OverwriteDuringSync_SnapshotExact |
| I-183 | `TestFSM_SnapshotDelete_TwoStep`, `TestMirror_DeleteReconciles_MarkAndSweep`, `TestMirror_SweepNeverTakesReachable`, `TestMirror_SweepKeepsActiveWrite`, `TestMirror_RowlessManifestImported`, `TestMirror_ForeignClusterUntouched`, `TestAdminOps_NoSnapshotDelete`, `TestSchedule_ThinThenSweep`, `TestMirror_OfflineDeleteReconciledOnAttach`, `TestFSM_SnapshotImport_Table`, `TestMirror_RowlessManifestsImportedOnTick`, `TestMirror_RenumberNeverReplaces`, `TestCLI_MirrorRemove_LeavesDisk`; planned: TestWebUI_NoSnapshotDeleteRoute |
| I-186 | `TestVFS_Revert_FromPoolAndMirror`, `TestVFS_Revert_FlushesDirtyFirst`, `TestVFS_SaveCopy`, `TestVFS_RestoreTree_SkipsExisting`, `TestVFS_Versions`, `TestAdminOps_RestoreAndVersions` |
