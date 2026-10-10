# 02 — Consistency and Raft

All metadata — the inode tree, the chunk table with placement and refcounts, the node table,
the volume policy and the cluster meta — is one deterministic state machine replicated with
`hashicorp/raft`. Chunk *data* is immutable and content-addressed, so it needs no consensus:
a shard either exists under its hash or it does not. This split is what keeps the design
small: Raft carries ~100 bytes per metadata operation; bulk bytes go point to point.

## What lives where

| In Raft (replicated, durable) | Local to a node (derived or private) |
|---|---|
| `State` from 01: inodes, chunk table, node table, volume policy, cluster meta | shard files, chunk cache, gateway WAL |
| placement of every chunk | liveness (`/health` probes), RTT ranking |
| node states Active/Draining/Removed, capacity, voter flag | S3 credentials, access keys, config |
| join token, cluster ID | Raft log and snapshots themselves (`raft.db`, `snapshots/`) |

## Node identity

- **Cluster node ID**: 16 random hex characters generated once and persisted at
  `<data_dir>/node_id`. It is the `raft.ServerID`. It never changes while the data directory
  exists; this is what Raft requires.
- **Tailscale stable ID**: recorded in `Node.TSStableID` and used only to authorize RPCs
  ([08](08-security.md)). It may change on re-auth; that is handled as a member update, not an
  identity change.
- **Address**: `Node.Addr` is the device's Tailscale IPv4. Raft addresses are `Addr:RaftPort`,
  not MagicDNS names (DNS can be down when tailscaled is down). On every probe cycle a node
  compares `tailscale status` Self IP with its own `Node.Addr`; if different it asks the
  leader to update both the Raft configuration (`AddVoter`/`AddNonvoter` with the same ID and
  the new address updates in place) and the node table. Because the source IP is by
  definition unknown to the member table at that moment, this one request (`POST
  /v1/members {update_addr}`) is authorized by `WhoIs(srcIP).StableID == Node.TSStableID`
  of the claimed node ID, not by IP lookup ([08](08-security.md)).
- **Cluster ID**: 16 random bytes created at `init`, stored in `Meta.ClusterID` and in
  `<data_dir>/cluster_id`. Every Raft stream exchanges it in a 16-byte handshake on dial and
  accept (a wrapper around `raft.StreamLayer`); mismatch closes the connection. Every RPC
  carries it in `X-Tailraid-Cluster`; mismatch is `409`. This prevents two independently
  `init`ed clusters on one tailnet from merging logs.

## Commands

Every command is proposed through `meta.Proposer` and applied by `meta.FSM.Apply`. Every
command carries `Now int64` (unix nanos) from the proposer; the FSM never reads a clock, the
disk, or iteration order of a map when deciding anything (I-4). The FSM keeps
`State.LastNow` and uses `now = max(cmd.Now, State.LastNow)` (then `LastNow = now`) wherever
a timestamp is recorded, so a proposer with a slow clock cannot back-date a tombstone or an
mtime; the FSM clock is monotonic across proposers (`TombstonedAt`, `Mtime`, `Ctime`,
`JoinedAt` all use it). Results are returned in
`Result{Ino, Gen, Victims, Err}` with `Err` one of the sentinel errors below — never a Go
panic or a non-deterministic error string.

```go
type Op uint8
const (
    OpSetVolume Op = iota + 1 // {ChunkSize (only at bootstrap), K, M, Now}
    OpMetaSet                 // {ClusterID, JoinToken, CreatedAt}
    OpNodeUpsert              // {Node fields; Idx 0 = allocate}
    OpNodeSetState            // {Idx, State Active|Draining|Removed, Now}
    OpCreate                  // {Parent, Name, Type TFile|TSymlink, Mode, Uid, Gid, Target, Now}
    OpMkdir                   // {Parent, Name, Mode, Uid, Gid, Now}
    OpUnlink                  // {Parent, Name, Now}
    OpRmdir                   // {Parent, Name, Now}
    OpRename                  // {OldParent, OldName, NewParent, NewName, Now}
    OpSetAttr                 // {Ino, Mask, Mode, Uid, Gid, Atime, Mtime, Now}
    OpCommit                  // {Ino, BaseVersion, HasTruncate, TruncateTo, NewSize, Writes []ChunkWrite, NewChunks []ChunkDesc, Now}
    OpSetPlacement            // {Hash, Placement []NodeIdx, K, M}   (repair / migration)
    OpGCDelete                // {Before int64} → Result.Victims
    OpSetVolumeMode           // {Auto bool, Now}   (03: automatic redundancy)
)
type ChunkWrite struct { Idx uint32; Hash Hash }                         // Hash may be HashZero
type ChunkDesc  struct { Hash Hash; Size uint32; K, M uint8; Placement []NodeIdx }
type GCVictim   struct { Hash Hash; Placement []NodeIdx }

var (
    ErrExist, ErrNotExist, ErrNotEmpty, ErrIsDir, ErrNotDir, ErrLoop,
    ErrConflict, ErrUnknownChunk, ErrNeedTailRewrite, ErrBadCommand,
    ErrNoSuchNode, ErrNodeNotEmpty, ErrPolicy error
)
```

### Apply rules

**SetVolume.** `ChunkSize` may only be set when `State.Vol.ChunkSize == 0` (bootstrap);
later commands must repeat the existing value or get `ErrPolicy`. `K >= 1`, `M >= 0`,
`K+M <= 256`. If `K` or `M` changes, `PolicyVersion++`. Existing chunks are untouched; the
repairer migrates them ([06](06-gc-and-repair.md)). This is a day-zero feature: the FSM
accepts it from the first log entry.

**SetVolumeMode.** Sets `Vol.Auto`; `PolicyVersion` is untouched. A node older than the
op rejects the entry (`ErrBadCommand`), so every node is upgraded before the mode is
used (12 §6).

**MetaSet.** Sets `Meta`. `ClusterID` may be set once; later `MetaSet` commands must carry
the same `ClusterID` or the zero value (meaning "unchanged"), else `ErrPolicy`. `token
rotate` is `MetaSet{ClusterID: zero, JoinToken: new}`.

**NodeUpsert.** `Idx == 0` first looks for a live record with the same node ID and updates
it (a join retried after a timeout, possibly at a new leader whose copy has not yet applied
the first upsert, must not create a second member); otherwise it allocates `NextNode`
(never reused) and sets `State = Active`.
Otherwise updates mutable fields (Name, TSStableID, Addr, RaftPort, RPCPort, Capacity, Voter,
Version, S3Endpoint, S3Bucket, S3Prefix, S3Region, S3PathStyle — the `Node` struct of 01) of
an existing node; `ErrNoSuchNode` if absent. `Kind` and `ID` cannot change.

**NodeSetState.** `Active ↔ Draining`, and `Active | Draining → Removed`. `Removed` is
terminal. The FSM refuses the transition to `Removed` with `ErrNodeNotEmpty` while *any*
chunk entry (live or tombstoned) still has a non-zero placement slot on this node (every
slot is a sole copy of its shard index, I-9). The CLI flow is `node drain` → wait for the
repairer to re-place every slot → `node remove`; `--force` first proposes `SetPlacement`
with slot 0 for every remaining slot, making the degradation explicit, then removes. Order
on the leader: `NodeSetState{Removed}` is committed first, then `raft.RemoveServer`, so the
node table never lists a server that Raft has forgotten. A removed node is refused when it
tries to join again until it has been `reset`.

**Create / Mkdir.** `Parent` must exist and be `TDir` (`ErrNotExist`/`ErrNotDir`). `Name`
must be non-empty, not `.` or `..`, contain no `/`, and be ≤ 255 bytes (`ErrBadCommand`).
Existing child ⇒ `ErrExist`. Allocates `Ino = NextIno++`, `Gen = uint32(logIndex)`, sets
times to `Now`, `Nlink` 1 (dirs: 2, and parent `Nlink++`). Returns `Ino, Gen`.

**Unlink.** Child must exist and not be `TDir` (`ErrIsDir`). Removes the child entry and
the inode. For each `Chunks[i] != HashZero`: `Chunk.Refs--`; if it reaches 0, set
`TombstonedAt = Now` and add to `Tombs`. Any handle to the inode now fails `FromHandle`
(I-3, I-17).

**Rmdir.** Child must be `TDir` (`ErrNotDir`) with empty `Children` (`ErrNotEmpty`). Removes
it; parent `Nlink--`.

**Rename.** Source must exist. If destination exists: file/symlink over file/symlink
replaces it (as Unlink of the destination, then move); dir over *empty* dir replaces it;
dir over non-empty dir ⇒ `ErrNotEmpty`; type mismatch ⇒ `ErrIsDir` or `ErrNotDir`. Moving a
directory into its own subtree ⇒ `ErrLoop` (walk `NewParent`'s parent chain to root and
reject if it passes through the source). Same path ⇒ no-op success. Only `Parent`, `Name`,
the two `Children` maps and `Ctime` change; `Ino`, `Gen` and the handle stay valid (I-16).

**SetAttr.** `Mask` selects Mode, Uid, Gid, Atime, Mtime. `Ctime = Now`. Size is *not*
settable here; truncation is a `Commit`.

**Commit** — the only command that changes file data. Applied atomically or not at all:

1. `inode` must exist and be `TFile` (`ErrNotExist`/`ErrIsDir`).
2. `inode.Version != BaseVersion` ⇒ `ErrConflict` (optimistic concurrency; the gateway
   rebases and retries, see 04).
3. Each `NewChunks[j]`: `Hash != HashZero`, `1 <= Size <= ChunkSize`, `K >= 1`,
   `len(Placement) == K+M`, every non-zero `Placement` entry is a distinct existing node
   (`ErrBadCommand` otherwise). If absent from `State.Chunks`, insert with `Refs = 0`. If
   present, the existing entry is kept (dedup); the command's placement is ignored.
4. Each `Writes[i].Hash` must be `HashZero` or present in `State.Chunks` (after step 3),
   else `ErrUnknownChunk`. Tombstoned chunks are valid targets (they get revived).
5. **Truncate first.** If `HasTruncate`: `t = ceil(TruncateTo/ChunkSize)`; `cur =
   old[:min(len(old), t)]`. If `TruncateTo < inode.Size`, `TruncateTo % ChunkSize != 0`
   and chunk `t-1` is neither in `Writes` nor currently `HashZero` ⇒ `ErrNeedTailRewrite`
   (the FSM cannot trim bytes out of a chunk; the flusher always rewrites the straddling
   chunk, see 04). Without `HasTruncate`, `cur = old` and `NewSize >= inode.Size` is
   required (`ErrBadCommand`): a file can only shrink through an explicit truncate.
6. **Then grow.** `NewSize` must not exceed `MaxFileSize(ChunkSize) = ChunkSize × 2^20`
   (`ErrBadCommand`, checked before anything is sized by it: the chunk list of every file
   lives in memory on every node, I-64). `n = ceil(NewSize/ChunkSize)`; `NewSize >= TruncateTo` when
   `HasTruncate`. `newChunks = copy(cur[:min(len(cur), n)])`, extended with `HashZero` to
   length `n`. Every `Writes[i].Idx` must be `< n` (`ErrBadCommand`).
7. **Then write.** Apply `Writes`. For each position whose hash changed (including positions
   dropped by the truncate): old hash (if not `HashZero`) `Refs--` with tombstone on zero;
   new hash (if not `HashZero`) `Refs++` and, if it was tombstoned, `TombstonedAt = 0` and
   remove from `Tombs`. A `Refs--` that would underflow is `ErrBadCommand` (never a panic).
8. `inode.Chunks = newChunks; Size = NewSize; Version++; Mtime = Ctime = now`.

**SetPlacement.** Chunk must exist (`ErrUnknownChunk`). Replaces `Placement`, `K`, `M`
(length must equal the new `K+M`; non-zero entries distinct existing nodes). Used by repair
to fill slots, re-place slots off draining/removed nodes, and migrate to a new policy.
`Expect` is the placement the proposer planned from: the command applies only if the
chunk still has it, else `ErrConflict` (I-102, L-197), so a flush filling slots, a repair
pass and a `--force` removal zeroing slots cannot silently undo each other; the loser
re-plans from the current placement or skips the chunk for this pass. The Raft index of
the applied `SetPlacement` (or `GCDelete`) is the fence carried by the shard `DELETE` RPC
([06](06-gc-and-repair.md)).

**GCDelete.** For every chunk in `Tombs` with `TombstonedAt < Before` (and `Refs == 0`,
which `Tombs` guarantees), in hash order and at most `Limit` of them (I-103; `0` is
unbounded): delete it from `Chunks` and `Tombs` and append `GCVictim{Hash, Placement}` to
`Result.Victims`. The leader then deletes the shards and proposes again while a batch
comes back full. Deterministic because `Before` and `Limit` are in the command and the
victim list is emitted in hash order.

**Codec versions.** Log entries are written in codec version 1, 2 (`NodeUpsert` with a
disk or host) or 3 (`SetPlacement.Expect`, `GCDelete.Limit`), snapshots in version 3;
every reader decodes every older version. `meta.CodecVersion` is the newest a binary
writes; `/health` reports it and `join` refuses a cluster that reports a newer one
(I-105, [12](12-operations.md) §6). An entry a binary cannot decode still advances its
applied index (so replicas stay aligned) and is counted and logged (I-105).

**Idempotency.** Raft may re-deliver entries on restore; `Apply` is pure with respect to the
log so re-applying from a snapshot's index yields the same state (I-5).

## FSM, snapshot, restore

```go
type FSM struct { mu sync.RWMutex; st *State; applied atomic.Uint64; cond *sync.Cond }
func (f *FSM) Apply(l *raft.Log) interface{}            // decode → dispatch → applied=l.Index → cond.Broadcast; returns Result
func (f *FSM) Snapshot() (raft.FSMSnapshot, error)     // RLock; deep-clone State; return &snap{clone}
func (f *FSM) Restore(rc io.ReadCloser) error          // DecodeState; swap under Lock; applied = st.Applied
func (s *snap) Persist(sink raft.SnapshotSink) error   // EncodeState(sink, clone); Cancel on error
func (f *FSM) WaitApplied(ctx context.Context, index uint64) error
```

`hashicorp/raft` guarantees `Apply` never runs concurrently with `Snapshot()` but *does* run
concurrently with `Persist`. Hence `Snapshot()` clones under the read lock (Children maps
deep-copied; `Chunks` slices shared because they are immutable) and `Persist` works on the
clone with no lock. Snapshots are written to `raft.FileSnapshotStore` (retain 2) and fsync
on close. Log and stable store are `raft-boltdb/v2` in `<data_dir>/raft/raft.db` with
`NoSync = false`.

Raft configuration: `HeartbeatTimeout 2s`, `ElectionTimeout 2s`, `LeaderLeaseTimeout 1s`,
`CommitTimeout 10ms` (how soon followers learn the commit index after an apply with no
successor; a follower gateway's `WaitApplied` bounds on it), `SnapshotInterval 2m`, `SnapshotThreshold 8192`, `TrailingLogs 10240`
(`cluster.Options.Timing` overrides the timeouts; the test harness uses 1 s / 1 s / 500 ms /
10 ms and Raft's in-memory stores through `Options.Stores`, which is how tests get speed
without any switch that skips an fsync).
Tests use `raft.NewInmemTransport`/`NewInmemStore` with 20 ms timeouts.

## Reads

`meta.Store` is the read API of the FSM (`type Store = FSM`); the vfs, gateways, repairer
and CLI reports hold a `*meta.Store`. Every read (`GetInode`, `Lookup`, `Resolve`, `PathOf`,
`ReadDir`, `GetChunk`, `ForEachChunk`, `Nodes`, `Volume`, `Stats`, `Applied`,
`WaitApplied`) is served from the local FSM under `RLock`, returning copies. An inode
copy carries no `Children` map (a directory's name table is read through `Lookup` and
`ReadDir`), so a LOOKUP or GETATTR costs O(1) however large the directory. Followers may therefore serve slightly stale
metadata; this is accepted NFS behaviour and lets a disconnected laptop browse the tree.
A gateway that just proposed a command calls `WaitApplied(index)` before answering, which
gives read-your-writes on the issuing node.

The **stale window** this creates is one replication round in normal operation and the
whole partition on a minority side: a gateway that has not yet applied another gateway's
unlink, rename or create resolves the *old* name table. A client there may open an inode
that is already unlinked (its later WRITEs are accepted into the WAL and dropped when the
Commit answers `ErrNotExist`, docs/04), see `ENOENT` for a file that exists elsewhere, or
create a name that the leader then reports as `ErrExist`. These are the NFS close-to-open
anomalies of any multi-server filesystem with asynchronous metadata; what never happens is
a committed byte changing because of them (I-12, I-35). The model checker encodes exactly
this rule: after a remove or rename nobody writes the affected names until every node has
caught up, and a definite answer from a stale gateway counts as "nothing happened".

Health, computed locally:

| `Health` | Condition | Gateway behaviour |
|---|---|---|
| `Healthy` | leader known, last contact < 30 s | normal |
| `Stale` | no leader contact for ≥ 30 s | reads from local FSM; metadata mutations (create, mkdir, unlink, rename, setattr) return `EROFS`, which reaches the wire as `NFS3ERR_ACCES` (create, mkdir, symlink, setattr) or `NFS3ERR_IO` (remove, rmdir, rename): go-nfs maps a filesystem error per operation and has no `ROFS` mapping (07); before the threshold a failed proposal is retried with backoff, since a leader change takes seconds; data WRITEs to existing files are still accepted into the WAL until the dirty caps (04), then `NFS3ERR_IO` |
| `NoLeader` | Raft has no leader (quorum lost) | same as `Stale`; `status` reports which voters are unreachable |

## Proposing and leader forwarding

`hashicorp/raft` only accepts `Apply` on the leader. `cluster.Cluster.Apply` applies locally
when leader, otherwise forwards the encoded command to the current leader with
`POST /v1/apply` over the node RPC. Semantics are exact:

- `ErrNotLeader` (the target was not leader when it received the command): the command was
  **not** applied; the caller retries against the new leader (bounded, with backoff).
- `ErrLeadershipLost`, timeout, a cancelled context while the future is pending, a
  transport error, or the leader's `500` after it handed the command to Raft: the outcome
  is **unknown**. Every such failure is reported as `meta.ErrUnknownOutcome` (wrapping the
  cause); the forwarding layer never retries it by itself (I-29) and the leader's `apply`
  handler never disguises it as "not the leader" (I-95). A context that is already expired
  is refused before dispatch, since Raft disarms its timer for a zero timeout.
- *Callers* resolve an unknown outcome instead of retrying blindly (I-95): the gateway
  proposes the command once more and reads the second answer knowing a first attempt may
  have landed. `Commit` carries `BaseVersion`, so the repeat either applies or returns
  `ErrConflict`, after which the flusher reloads the inode and re-plans. `Create` and
  `Mkdir` may hit `ErrExist`, which the gateway maps to success when the existing child
  matches; a `Symlink` only when the existing link has the same target. `Unlink`, `Rmdir`
  and `Rename` take `ErrNotExist` after an unknown outcome as the first attempt having
  landed. `SetAttr` sets absolute values and is simply repeated. A second unknown outcome
  in a row is `EIO`. Commands whose first proposal was *not* applied (`ErrNotLeader`,
  `ErrNoLeader`) are retried with backoff until the gateway is stale.
- A leader with no quorum eventually steps down; proposers see `ErrNoLeader` and the flusher
  backs off (data stays in the WAL, see 04).

## Membership

**Bootstrap** (`tailraid init`): refuses if `<data_dir>/raft` has state. Generates
`node_id`, `cluster_id`; `raft.BootstrapCluster` with self as the only voter; applies
`MetaSet`, `SetVolume{ChunkSize, 1, 0}`, `SetVolumeMode{Auto: true}`, `NodeUpsert(self)`
(no budget: a device stores nothing, 14 §1) and a `NodeUpsert` per configured disk with
its derived budget (01). The node is named after the host; `--name`, `--chunk` and
`--token` are the only options.

**Join** (`tailraid join <host> <token> [--voter]`): refuses if local Raft state exists.
Generates `node_id`; `POST /v1/join` with `{token, node_id, name, ts_stable_id, addr, ports,
voter, version}`. The receiver requires `WhoIs(srcIP).StableID == ts_stable_id`
and `token == Meta.JoinToken`, then forwards to the leader, which calls
`raft.AddNonvoter` or `AddVoter` and proposes `NodeUpsert`. The joiner then starts `serve`
and receives the state by snapshot, after registering its configured disks through the
leader's `add_disk` member op (14 §2, I-121).

**Disks.** `node add-disk` (14 §2) proposes `NodeUpsert{Kind: KindDisk, Host}` through
the leader; a disk row is a placement target, never a Raft member, and `init` registers
the disks already in the config in its bootstrap.

**Voters.** Only devices configured `voter: true` (and the `init` node) are Raft voters.
Non-voters replicate the log, serve reads and act as gateways and storage, but do not count
toward quorum, so sleeping laptops never stall the cluster. `status` warns when the voter
count is 1 or even.

**Lifecycle commands** (all day-zero): `node promote|demote` (`AddVoter`/`AddNonvoter` on the
same ID), a disk's budget (`set_capacity` → `NodeUpsert`, proposed by the disk's host as
its free space changes, 01; refused for a device row, I-118), `node drain` (`NodeSetState Draining`:
excluded from new placement; repairer re-places its slots), `node remove`
(`NodeSetState Removed`, then `raft.RemoveServer`), `set_version` (`NodeUpsert` with the
version `serve` runs: proposed by the node at start, through the leader, whenever its row
names another, so `node ls` shows what each node runs rather than what it joined with,
I-133), `volume set-redundancy` (`SetVolume`
after `SetVolumeMode{false}`; `--auto` is `SetVolumeMode{true}`),
`tailraid reset --yes` (local only: deletes `<data_dir>/raft` and `node_id`/`cluster_id` so
the device can `join` afresh; never touches `shards/`, which the sweep reconciles later; it
refuses while `serve` runs and while `staging/` holds unflushed writes, unless
`--discard-dirty` accepts losing them).

## Partitions and quorum

Raft guarantees that only a partition containing a majority of voters elects a leader and
commits entries (I-6). The minority side's gateways move to `Stale`/`NoLeader`: reads
continue from the local FSM and from any reachable shard holders; metadata mutations return
`EROFS` (`NFS3ERR_ACCES`/`NFS3ERR_IO` on the wire, 07); data WRITEs are still accepted into the WAL until the dirty caps, and
everything in the WAL sits there until the flusher can propose again.
When the partition heals, the minority's Raft log is reconciled by Raft itself; our FSM has
no divergent state to merge because it never applied uncommitted entries.

## Invariants

- **I-4** — The FSM is deterministic: applying the same sequence of log entries to any number
  of fresh FSMs yields byte-identical `EncodeState` output; `Apply` never consults a clock,
  the filesystem, randomness, or Go map iteration order.
- **I-5** — Snapshot and restore are equivalent to replay: for any prefix of the log,
  `Restore(Snapshot(prefix))` followed by applying the remaining entries yields the same
  state as applying the whole log.
- **I-6** — Under any network partition, metadata operations are committed on at most one
  side (the one with a voter majority); a gateway on the minority side never reports a
  metadata write as successful.
- **I-26** — Every failure of a command is reported as one of the sentinel errors in
  `Result.Err`, and a failed command leaves `State` unchanged.
- **I-27** — Reference counts are exact: for every chunk, `Refs` equals the number of
  `(inode, index)` positions whose hash is that chunk, and `Tombs` is exactly the set of
  chunks with `Refs == 0`.
- **I-28** — A `Commit` whose `BaseVersion` does not match the inode's current `Version` is
  rejected without side effects, so two gateways cannot interleave partial updates to one
  file.
- **I-62** — A node ID has at most one record that is not `Removed` in the node table: a `NodeUpsert` with `Idx == 0` for a known ID updates that record and never allocates a new index, so a retried join is idempotent whichever leader serves it.
- **I-64** — No command makes a file larger than `MaxFileSize(ChunkSize)` (`ChunkSize × 2^20` chunks): a `Commit` beyond it is `ErrBadCommand` before anything is allocated from its size, the gateway refuses the WRITE or SETATTR with `EFBIG` first, and `Apply` never panics on a decodable command.
- **I-29** — The leader-forwarding layer never applies a command twice: a forwarded command
  is handed to Raft at most once per proposal, and the forwarding layer itself retries only
  after an explicit `ErrNotLeader`. (Callers may retry idempotent commands as described
  above; that is outside this invariant.)
- **I-96** — `node_id` and `cluster_id` are written atomically (temp file, fsync, rename,
  directory fsync), so a crash leaves the old file or the new one; a read error other
  than "not found" on `node_id` is an error, never a fresh identity.
- **I-99** — A node that fails to open or bootstrap releases its Raft listener and its
  stores before returning the error, so the same data directory and address can be
  opened again at once.
- **I-102** — A `SetPlacement` whose `Expect` differs from the chunk's current placement
  is rejected with `ErrConflict` and changes nothing, so two proposers that planned from
  different placements never overwrite each other's slots unnoticed.
- **I-103** — A `GCDelete` removes at most `Limit` entries, the lowest hashes first, and
  the GC worker proposes again while a batch comes back full, so a backlog of any size
  drains in bounded applies and every replica removes the same entries per entry.
- **I-104** — A snapshot is a deep copy of the mutable state taken under the read lock,
  and chunk lists are immutable (a `Commit` installs a new list, never rewrites one): a
  `Commit` applied after the snapshot never changes it, and a snapshot is written and read
  as a stream, never held whole in memory a second time.
- **I-105** — A log entry this binary cannot decode is counted (`status` shows
  `incompatible_entries` and a warning) and logged, while the applied index advances as on
  every replica; `/health` reports `meta.CodecVersion`, and `join` refuses a cluster whose
  node reports a newer one.
- **I-95** — A proposal that may have been applied (leadership lost, timeout, cancelled
  while pending, transport error, or the leader's `500`) is reported as
  `meta.ErrUnknownOutcome` and never as "not the leader"; the gateway re-proposes it once
  and resolves the second answer per command (`ErrExist`/`ErrNotExist` after an unknown
  outcome count as the first attempt having landed), so an acknowledged mutation is never
  reported as failed and a failed one is never silently repeated. No proposal outlives
  its node: `Close` refuses new proposals and, on a leader, drains what Raft committed
  into the FSM (a barrier) before Raft stops, since raft's FSM loop drops its queue on
  shutdown and a future for a committed-but-unapplied entry would otherwise never
  resolve; every waiter returns and no goroutine is left behind.
- **I-133** — A node's row carries the version its `serve` runs: at start a node whose
  row names another version proposes `set_version` through the leader, retrying until
  one accepts it, and a row that already matches is never rewritten; `set_version`
  changes nothing but the version.

## Tests

| Invariant | Tests |
|---|---|
| I-4 | `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`, `TestFSM_ApplySnapshotRestoreEquivalence`, `TestFSM_DeterministicAcrossReplicas`, `TestModelCheck_Long` |
| I-5 | `TestCluster_RejoinAfterLongAbsence_SnapshotInstall`, `TestFSM_ApplySnapshotRestoreEquivalence`, `TestSingleNode_BootstrapProposeRestart` |
| I-6 | `TestCluster_SplitBrain_OnlyMajorityCommits` |
| I-26 | `FuzzDecodeCommand`, `TestEncodeCommand_BadCommandIsError`, `TestErrorCode_RoundTrip`, `TestFSM_BadCommandLeavesStateUnchanged`, `TestFSM_Commit_NewSizeBounds`, `TestFSM_Commit_RejectsImplicitShrink`, `TestFSM_Create_NameBoundaries`, `TestFSM_NodeRemove_RefusedWhileHoldingSlots`, `TestVFS_RenameIntoOwnSubtree_NoSideEffects` |
| I-27 | `TestCheckInvariants_DetectsEachViolation`, `TestFSM_Commit_NewFileAndOverwrite`, `TestFSM_Commit_SameHashTwiceInOneFile_RefsPerIndex`, `TestFSM_Revive` |
| I-28 | `TestFSM_Commit_RejectsStaleBaseVersion` |
| I-62 | `TestFSM_NodeUpsert_IdempotentByID` |
| I-95 | `TestVFS_UnknownOutcome_ResolvesPerCommand`, `TestApply_UnknownOutcomeIs500`, `TestApplyLocal_ExpiredAndCancelledContext`, `TestCluster_CloseWhileApplying_EveryWaiterResolves` |
| I-96 | `TestLoadOrCreateNodeID_ReadErrorIsNotANewIdentity` |
| I-102 | `TestFSM_SetPlacement_ExpectConflict`, `TestEncodeCommand_Golden` |
| I-103 | `TestFSM_GCDelete_LimitBoundary`, `TestGC_DrainsBacklogInBatches` |
| I-104 | `TestFSM_SnapshotUnaffectedByLaterCommit`, `TestDecodeState_StreamsFromAnyReader` |
| I-105 | `TestFSM_IncompatibleEntryCounted`, `TestCodecCompatible`, `TestMembersBadSlotHealthStatus` |
| I-99 | `TestOpenFresh_StartFailureFreesPortAndStores` |
| I-64 | `TestChunkCount_NoOverflow`, `TestFSM_Commit_NewSizeBounds`, `TestVFS_SizeBeyondMax_EFBIG` |
| I-29 | `TestApply_ForwardAndRedirect`, `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`, `TestFSM_WaitApplied_ReturnsOnClose`, `TestModelCheck_Long` |
| I-133 | `TestMember_SetVersion`, `TestReportVersion_UpdatesOwnRow` |
