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

**MetaSet.** Sets `Meta`. `ClusterID` may be set once; later `MetaSet` commands must carry
the same `ClusterID` or the zero value (meaning "unchanged"), else `ErrPolicy`. `token
rotate` is `MetaSet{ClusterID: zero, JoinToken: new}`.

**NodeUpsert.** `Idx == 0` allocates `NextNode` (never reused) and sets `State = Active`.
Otherwise updates mutable fields (Name, TSStableID, Addr, RaftPort, RPCPort, Capacity, Voter,
Version, S3Endpoint, S3Bucket, S3Prefix, S3Region, S3PathStyle — the `Node` struct of 01) of
an existing node; `ErrNoSuchNode` if absent. `Kind` and `ID` cannot change.

**NodeSetState.** `Active → Draining → Removed` or `Draining → Active`. `Removed` is terminal.
Transition to `Removed` is refused with `ErrNodeNotEmpty` while *any* live chunk still has a
non-zero placement slot on this node (every slot is a sole copy of its shard index, I-9). The
CLI flow is `node drain` → wait for the repairer to re-place every slot → `node remove`;
`--force` first proposes `SetPlacement` with slot 0 for the remaining shards, making the
degradation explicit, then removes. Order on the leader: `NodeSetState{Removed}` is committed
first, then `raft.RemoveServer`, so the node table never lists a server that Raft has
forgotten.

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
6. **Then grow.** `n = ceil(NewSize/ChunkSize)`; `NewSize >= TruncateTo` when
   `HasTruncate`. `newChunks = copy(cur[:min(len(cur), n)])`, extended with `HashZero` to
   length `n`. Every `Writes[i].Idx` must be `< n` (`ErrBadCommand`).
7. **Then write.** Apply `Writes`. For each position whose hash changed (including positions
   dropped by the truncate): old hash (if not `HashZero`) `Refs--` with tombstone on zero;
   new hash (if not `HashZero`) `Refs++` and, if it was tombstoned, `TombstonedAt = 0` and
   remove from `Tombs`. A `Refs--` that would underflow is `ErrBadCommand` (never a panic).
8. `inode.Chunks = newChunks; Size = NewSize; Version++; Mtime = Ctime = now`.

**SetPlacement.** Chunk must exist (`ErrUnknownChunk`). Replaces `Placement`, `K`, `M`
(length must equal the new `K+M`; non-zero entries distinct existing nodes). Used by repair
to fill slots, re-place slots off draining/removed nodes, and migrate to a new policy. The
Raft index of the applied `SetPlacement` (or `GCDelete`) is the fence carried by the shard
`DELETE` RPC ([06](06-gc-and-repair.md)).

**GCDelete.** For every chunk in `Tombs` with `TombstonedAt < Before` (and `Refs == 0`,
which `Tombs` guarantees): delete it from `Chunks` and `Tombs` and append
`GCVictim{Hash, Placement}` to `Result.Victims`. The leader then deletes the shards.
Deterministic because `Before` is in the command and the victim list is emitted in hash
order.

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
`CommitTimeout 50ms`, `SnapshotInterval 2m`, `SnapshotThreshold 8192`, `TrailingLogs 10240`.
Tests use `raft.NewInmemTransport`/`NewInmemStore` with 20 ms timeouts.

## Reads

`meta.Store` is the read API of the FSM (`type Store = FSM`); the vfs, gateways, repairer
and CLI reports hold a `*meta.Store`. Every read (`GetInode`, `Lookup`, `Resolve`, `PathOf`,
`ReadDir`, `GetChunk`, `ForEachChunk`, `Nodes`, `Volume`, `Stats`, `Applied`,
`WaitApplied`) is served from the local FSM under `RLock`, returning copies. Followers may therefore serve slightly stale
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
| `Stale` | no leader contact for ≥ 30 s | reads from local FSM; metadata mutations (create, mkdir, unlink, rename, setattr) return `NFS3ERR_ROFS`; data WRITEs to existing files are still accepted into the WAL until the dirty caps (04), then `NFS3ERR_IO` |
| `NoLeader` | Raft has no leader (quorum lost) | same as `Stale`; `status` reports which voters are unreachable |

## Proposing and leader forwarding

`hashicorp/raft` only accepts `Apply` on the leader. `cluster.Cluster.Apply` applies locally
when leader, otherwise forwards the encoded command to the current leader with
`POST /v1/apply` over the node RPC. Semantics are exact:

- `ErrNotLeader` (the target was not leader when it received the command): the command was
  **not** applied; the caller retries against the new leader (bounded, with backoff).
- `ErrLeadershipLost`, timeout, or transport error after the command was handed to Raft: the
  outcome is **unknown**; the forwarding layer returns the error and never retries by
  itself (I-29). *Callers* may retry commands that are idempotent by construction: for
  `Commit`, `BaseVersion` makes a blind retry either succeed once or fail with
  `ErrConflict`, after which the gateway reloads the inode and re-plans; for
  `Create`/`Mkdir` a retry may hit `ErrExist`, which the gateway maps to success if the
  existing child matches.
- A leader with no quorum eventually steps down; proposers see `ErrNoLeader` and the flusher
  backs off (data stays in the WAL, see 04).

## Membership

**Bootstrap** (`tailraid init`): refuses if `<data_dir>/raft` has state. Generates
`node_id`, `cluster_id`; `raft.BootstrapCluster` with self as the only voter; applies
`MetaSet`, `NodeUpsert(self)`, `SetVolume{ChunkSize, K, M}`.

**Join** (`tailraid join <host> <token> [--voter]`): refuses if local Raft state exists.
Generates `node_id`; `POST /v1/join` with `{token, node_id, name, ts_stable_id, addr, ports,
voter, capacity, version}`. The receiver requires `WhoIs(srcIP).StableID == ts_stable_id`
and `token == Meta.JoinToken`, then forwards to the leader, which calls
`raft.AddNonvoter` or `AddVoter` and proposes `NodeUpsert`. The joiner then starts `serve`
and receives the state by snapshot.

**Voters.** Only devices configured `voter: true` (and the `init` node) are Raft voters.
Non-voters replicate the log, serve reads and act as gateways and storage, but do not count
toward quorum, so sleeping laptops never stall the cluster. `status` warns when the voter
count is 1 or even.

**Lifecycle commands** (all day-zero): `node promote|demote` (`AddVoter`/`AddNonvoter` on the
same ID), `node set-capacity` (`NodeUpsert`), `node drain` (`NodeSetState Draining`:
excluded from new placement; repairer re-places its slots), `node remove`
(`NodeSetState Removed`, then `raft.RemoveServer`), `volume set-redundancy` (`SetVolume`),
`tailraid reset --yes` (local only: deletes `<data_dir>/raft` and `node_id`/`cluster_id` so
the device can `join` afresh; never touches `shards/`, which the sweep reconciles later).

## Partitions and quorum

Raft guarantees that only a partition containing a majority of voters elects a leader and
commits entries (I-6). The minority side's gateways move to `Stale`/`NoLeader`: reads
continue from the local FSM and from any reachable shard holders; metadata mutations return
`NFS3ERR_ROFS`; data WRITEs are still accepted into the WAL until the dirty caps, and
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
- **I-29** — The leader-forwarding layer never applies a command twice: a forwarded command
  is handed to Raft at most once per proposal, and the forwarding layer itself retries only
  after an explicit `ErrNotLeader`. (Callers may retry idempotent commands as described
  above; that is outside this invariant.)

## Tests

| Invariant | Tests |
|---|---|
| I-4 | `TestFSM_DeterministicAcrossReplicas`, `TestFSM_ApplyUsesCommandNowNotClock`, `TestCluster_PlacementAgreesOnAllNodes`, `TestSoak_FSMHashAgreement` |
| I-5 | `TestFSM_ApplySnapshotRestoreEquivalence`, `TestFSM_SnapshotPersistConcurrentWithApply`, `TestCluster_RejoinAfterLongAbsence_SnapshotInstall` |
| I-6 | `TestCluster_SplitBrain_OnlyMajorityCommits`, `TestCluster_MinorityGatewayReturnsEROFS`, `TestModelCheck_Long` |
| I-26 | `TestFSM_ApplyRules_Table`, `TestFSM_FailedCommandLeavesStateUnchanged`, `FuzzDecodeCommand` |
| I-27 | `TestFSM_RefCount_SharedChunkAcrossFiles`, `TestFSM_Invariants_RandomCommands`, `TestFSM_Tombstone_OnRefsZero`, `TestFSM_Commit_RevivesTombstonedChunk` |
| I-28 | `TestFSM_Commit_RejectsStaleBaseVersion`, `TestVFS_ConcurrentGateways_DisjointChunksBothLand`, `TestVFS_ConcurrentGateways_SameChunkOneWins` |
| I-29 | `TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`, `TestCluster_ForwardRetriesOnlyOnNotLeader`, `TestFSM_RefcountUnderflow_IsBadCommand` |
