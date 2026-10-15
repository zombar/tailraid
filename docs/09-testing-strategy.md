# 09 — Testing strategy

tailraid exists because its predecessor shipped a filesystem that corrupted every file
larger than one NFS WRITE, with 50k lines of tests that never wrote at a non-zero offset.
This document is the method that prevents a repeat: every behaviour the system promises is
a numbered invariant, every invariant has named tests, the tests exist before the code, and
a script refuses to let the mapping drift.

This document defines no invariants of its own; it defines how the others are proven.

## 1. Invariant discipline

- Each design doc ends with `## Invariants` (lines `- **I-N** — statement`) and `## Tests`
  (table mapping each I-N to test names). Numbers are globally unique and never reused.
- Every test that defends an invariant carries, on the line(s) above `func Test…`,
  `// Invariant: I-7, I-12`.
- `scripts/check-invariants.sh` (run by `make lint` and CI):
  - collects definitions from `docs/*.md` and citations from every `*_test.go`;
  - fails if any defined I-N has zero citations, any cited I-N is undefined, or any I-N is
    defined twice;
  - regenerates `docs/invariants.md` (Invariant | Doc | Statement | Tests) and, with
    `--check`, fails if it differs from the committed file.
- At M0 every invariant is cited by a skipped stub in `test/invariants/stubs_test.go`.
  A stub is deleted in the same commit that lands the real test.

**Lifecycle cells (`L-N`).** `docs/11-chunk-lifecycle.md` describes a chunk's states
(uploading, committed, degraded, repairing, tombstoned, …) as transition tables under
`## Transition table` and `## Forbidden transitions`. Each row's first cell is an ID `L-N`.
The same discipline applies: every cell is cited by at least one test via
`// Lifecycle: L-3, L-4` above the test func, or `// Regression: #<issue> L-3` for a test
added after a bug (the regression form counts as a citation and records provenance). The
script applies identical rules to L-N and regenerates `docs/lifecycle.md`
(Cell | From | Event | To / Outcome | Tests). Stubs live in
`test/invariants/lifecycle_stubs_test.go` until the real tests land.

## 2. TDD workflow

Per milestone: write the acceptance test first (it is red and stays `t.Skip`-free from the
moment the milestone starts). Per package, for each invariant that touches it: write the
test (red) → write the minimal code (green) → refactor with `-race` still green.

Rules that keep it honest:

- A `_test.go` that cites an invariant lands in the same commit as, or earlier than, the
  implementation it covers.
- `make check` runs in the lefthook pre-commit hook (`make hooks` installs it; lefthook is
  a Go tool in `tools.mod`) and must pass before every commit: `fmt-check`,
  `vendor-check`, `lint` (golangci-lint with the linters listed in `.golangci.yml`,
  `check-invariants.sh --check`, `deadcode`, `docs-check` for links, anchors and invariant
  mentions in the prose, `spell` with misspell over docs, TypeScript, CSS and the lookdev
  HTML, `actionlint` for the workflow, `yaml-check` and `shell-check`), `web-check` (the
  committed bundle), `web-typecheck` (typescript-go over `internal/webui/tsconfig.json`,
  since esbuild only bundles) and `go test -short -race ./...`. The CSS and the lookdev
  have no linter as a Go module, so `internal/webui/static_test.go` is theirs: every class
  the TypeScript emits exists in `app.css` or the lookdev, and every class `app.css`
  defines is used. The `commit-msg` hook requires the `scope: subject` form that
  `scripts/changelog.sh` groups by; `pre-push` runs `make vuln`.
- No coverage gate. `make test` prints per-package coverage so regressions are visible.
- A cited test never skips unconditionally: `check-invariants.sh` refuses a `t.Skip` at
  the top level of a test body outside `test/invariants`, so an invariant cannot count as
  covered by a test that proves nothing; a skip inside a condition (`-short`, an OS, a
  missing tool) is fine, and a test whose precondition depends on a hash vector writes
  until the precondition holds instead of skipping.
- Review checklist (also in `CLAUDE.md`): every write-path test includes a non-zero offset;
  every size-bounded path has a `{0, 1, max−1, max, max+1}` table; every "same answer on
  every node" function has an N-node determinism test; every error path returns a typed
  error that a test asserts with `errors.Is`.
- **No test-mode switches in production code.** No `TAILRAID_TEST` env var, no
  "skip fsync in tests" flag (forbidigo lint enforces the env var ban). Tests that need
  speed use in-memory stores; tests that prove durability use real temp directories.

## 3. Seams (the only ones)

| Seam | Where | Why |
|---|---|---|
| `clock.Clock` (`Now`, `NewTimer`, `After`, `Since`) + `clock.Fake{Advance, Set}` | every tailraid timer: GC grace, repair hold-down, flush idle, attr TTL, HTTP timeouts | no sleeps in tests; ~80 lines. Raft keeps its own real clock; raft tests use 20 ms timeouts and condition polling |
| `sysfs.FS` (`CreateTemp, OpenFile, Rename, Remove, Open, ReadDir, SyncDir`; file `Write, WriteAt, Sync, Close, Truncate`) + `sysfstest.Fault` | `internal/chunk` and the `internal/vfs` WAL only | ENOSPC, torn writes, failed fsync, crash at each fsync step (`SyncDir` is on the ack path: a PUT is acked only after it; a WAL append is acked only after its fsync) |
| `FaultNet` (`http.RoundTripper` wrapper keyed src→dst: `Drop`, `Delay`, `Partition`, `ErrorOnce`) | RPC client in tests and under `--dev` | production HTTP code runs in every multi-node test |
| `cluster.Hooks` (nil-checked func fields, one per crash point C1..C7 of `docs/05`: `BeforeWALSync`, `BeforeClientAck`, `AfterShardTempWritten`, `AfterShardFsync`, `AfterShardRename`, `AfterAllShardsAcked`, `AfterRaftCommit`) | write path | crash injection at every durability point; nil in production, no behaviour change |
| `tailscale.Client` interface + `tailscale.Fake` | WhoIs authorizer, prober | tailscaled is not available in CI |
| Raft: `raft.NewInmemTransport`, `raft.NewInmemStore`, `raft.NewInmemSnapshotStore` | cluster tests | raft is not wrapped; its own fakes are used |

Not seams: no mock shard store or mock FSM inside vfs tests (use the real ones over an
in-memory `sysfs`), no interface over Raft, no dependency-injected loggers.

## 4. Test pyramid by layer

Named tests below are the acceptance set; each cites its invariants in code.

**`internal/ec`** — `TestEncodeDecode_Sizes` over {0, 1, k−1, k, k+1, stripe−1, stripe,
stripe+1, 2·stripe−1} × (1,0), (1,2), (2,1), (4,2), (8,3); `TestDecode_EverySubsetOfKShards`
(all C(k+m,k) subsets); `TestDecode_FewerThanK_Fails`; `TestTailPadding_NotLeaked`;
`TestShardSizesEqual_AndDeterministic` (byte-identical shards on re-encode, required for content
addressing); `FuzzRoundTrip`.

**`internal/chunk`** — `TestStore_PutGetRoundTrip` size table; `TestStore_GetVerifiesHash_Corrupted`
(table: bit flip, truncated, zero-filled, bad magic → `ErrCorrupt` and the file is quarantined);
`TestChunkRPC_HashMismatchRejected`; `TestStore_CrashBeforeRename_LeavesNoVisibleShard`;
`TestStore_CrashAfterRenameBeforeDirSync_ShardStillValid`; `TestStore_CrashBeforeRename_LeavesNoVisibleShard`;
`TestStore_TornWrite_DetectedOnRead`; `TestStore_ConcurrentPutSameHash` (32 goroutines,
`-race`); `TestStore_DiskFull_ReturnsErrAndNoPartialShard`; `TestStore_PutDifferentPayloadSameKey_Replaces`;
`TestStore_DeleteIfOlder_RefusesYoungShard`; `TestCache_LRUEvictsBySize`; `FuzzStore_PutGet`.

**`internal/meta`** — `TestFSM_ApplySnapshotRestoreEquivalence` (5k random commands;
snapshot; restore; compare; also restore-then-replay-tail); `TestFSM_DeterministicAcrossReplicas`
(same log into 3 FSMs → byte-identical snapshots; catches map-iteration order);
rename table (`over file`, `over empty dir`, `over non-empty dir`, `into own subtree`,
`same path`, `cross-dir keeps ino+gen`); `TestHandle_RoundTrip_BigEndian`;
`TestFSM_InoGenNotReusedAfterUnlink`; `TestFSM_Commit_SameHashTwiceInOneFile_RefsPerIndex`;
`TestFSM_Commit_RejectsStaleBaseVersion`; `TestFSM_Commit_TruncateDown`;
`TestFSM_Commit_TruncateThenGrow_DropsOldBytes`; `TestFSM_LastNowMonotonic`;
`TestFSM_RefCountNeverUnderflows`;
`TestFSM_SetVolume_PolicyVersionBumps`; invariant checker used by every FSM test
(Σrefs = references from inodes, `len(Chunks) == ceil(Size/CS)`, parent chains reach root,
`Tombs ⇔ Refs==0`); `FuzzDecodeCommand`, `FuzzDecodeHandle`. Every parsing surface has a fuzz target (`make fuzz` runs them all for 30 s each): commands, snapshots, placement, shard keys and headers, NFS handles, the WAL (`FuzzWALReplay`), the release checksum file (`FuzzParseSums`).

**`internal/meta` placement** — `TestPlace_DeterministicAcrossNodes` (7 instances, shuffled
candidate order, 10k hashes → identical); `TestPlace_DistinctNodes`;
`TestPlace_FewerCandidatesThanN` (shorter result; caller fills 0); `TestPlace_CapacityWeighting`
(±10 % over 100k); `TestPlace_StabilityOnRemove` and `TestPlace_StabilityOnAdd` (adding one
node moves ≤ 1/N + ε of shards);
`FuzzPlace_Determinism`.

**`internal/vfs`** — go-billy's `BasicSuite` and `DirSuite` through a gocheck shim;
**`TestVFS_OracleRandomOps`**: a model (`map[path][]byte`) versus the VFS, seeded, 2000 ops
under `-short` and 50k in long mode, ops weighted over `OpenFile+Seek+Write+Close` at offsets
{0, EOF, EOF+gap, random inside, chunk boundary −1/0/+1}, `ReadAt`, `Truncate` up/down,
`Create`, `Rename`, `Remove`, `Mkdir`, `ReadDir`; full content compared after every op, whole
tree every 100 ops, and again after flush + reopen from the same stores (restart equivalence).
Plus the deliberately simple `TestVFS_WriteAtNonZeroOffset_PreservesPrefix`,
`TestVFS_PerRPCOpenCloseSequence` (the exact go-nfs per-WRITE sequence),
`TestOverlay_WriteSpanningChunkBoundary`, `TestOverlay_WritePastEOF_ReadsZerosInGap`,
`TestFSM_Commit_TruncateDown`, `TestOverlay_TruncateThenGrow_ZerosOldBytes`,
`TestVFS_PerRPCOpenCloseSequence`, `TestFlusher_DedupLiveAndTombstoned_NoUpload`,
`TestVFS_TruncateThenGrow_OldBytesDoNotResurface`,
`TestFlusher_ConflictRebase_PreservesConcurrentAppend`, `TestFlusher_UnknownChunk_ReUpload`,
`TestFlusher_DegradedCommitRecordsZeroSlots`, `TestFlusher_ConflictRebase_PreservesConcurrentAppend`,
`TestFlusher_ConflictRebase_PreservesConcurrentAppend`, `TestFlusher_PartialFlush_SizeNeverAheadOfData`,
`TestWAL_ReplayStopsAtTornTail`, `TestWAL_Compact_KeepsOnlyLive`,
`TestVFS_RemoveDirtyFile_DropsOverlay`, `TestOverlay_ConcurrentWritersDifferentInodes`.

**`internal/nfsgw`** — in-process over `willscott/go-nfs-client` against `nfs.Serve` on
`127.0.0.1:0` (no root): `TestNFS_WriteSequence_MultiRPC` (64 KiB and 1 MiB pieces, shuffled
offsets), `TestNFS_SetattrSizeTruncates`, `TestNFS_SetattrOwnerAndTimes`,
`TestNFS_RenameKeepsHandleValid`, 
`TestNFS_RemovedHandleIsStale`, 
`TestNFS_ReadDirPlusLargeDirectory` (5k entries), `TestNFS_FSStatAndUnsupported`.

**`internal/rpc`** — `TestChunkRPC_PutGetHeadDelete_SizeBoundaries` {0, 1, max−1, max, max+1 → 413} with client
and server limits asserted equal; `TestChunkRPC_HashMismatchRejected`; `TestChunkRPC_PutGetHeadDelete_SizeBoundaries`;
`TestChunkRPC_DeleteFence`,
`TestChunkRPC_DeleteFence`, ;
`TestWhoIs_DeniesUnknownPeer_AndStableIDMismatch_AndPurge`,
`TestWhoIs_DeniesUnknownPeer_AndStableIDMismatch_AndPurge`, ;
`TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader`; ;
`TestListen_RefusesNonLoopbackWithoutAuthorizer`.

**`internal/cluster` + `internal/testharness`** — harness:

```go
type Cluster struct { Nodes []*Node; Net *FaultNet; Clock *clock.Fake }
func NewCluster(t *testing.T, n int, opts ...Option) *Cluster   // K(k,m), Durable(), Seed(int64)
func (c *Cluster) Leader(t *testing.T) *Node
func (c *Cluster) Kill(i int); func (c *Cluster) Restart(t *testing.T, i int)
func (c *Cluster) Partition(a, b []int); func (c *Cluster) Heal()
func (c *Cluster) Quiesce(t *testing.T)   // Barrier on leader + all FSMs at the same index
```

Scenarios: `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes`, `TestCluster_KillMNodes_AllDataReadable_KillMPlusOne_EIO`,
`TestCluster_KillMNodes_AllDataReadable_KillMPlusOne_EIO`, `TestRepair_DeadNodeHoldDown_ThenReplaced`,
`TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`,
`TestCluster_RejoinAfterLongAbsence_SnapshotInstall`, `TestCluster_GCDoesNotDeleteInflightUpload`,
`TestCluster_SplitBrain_OnlyMajorityCommits`, `TestModelCheck_Long`,
`TestIDStream_RejectsForeignCluster`, 
`TestJoin_TokenAndIdentity`, `TestCluster_UpdateAddr_ChangesRPCAddr`,
`TestRepair_DeadNodeHoldDown_ThenReplaced`, `TestSecondOpen_IsRefused`.

**Data-loss specific** — `TestCrash_AtEveryPoint`: for each hook, start a write of a known
payload, kill the gateway at the hook, restart, and require exactly one of two outcomes:
not acknowledged ⇒ old content and no visible orphan shards, or acknowledged ⇒ new content
readable from every live gateway. `TestRepair_ReportedBadSlot_AndScrub_RepairCorruption`.
`TestDurability_AckImpliesReadableFromKSurvivors` (`Durable()`; after each ack: kill and
restart the gateway, let the WAL replay and flush, then kill m nodes and read elsewhere;
rotate the gateway; 20 iterations short / 200 long). **`TestModelCheck_Long`** (skipped under `-short`; `make
test-long`): 5 nodes 3+2, `Durable()`, a seeded scheduler of client ops, `Kill`, `Restart`,
`Partition`, `Heal`, `Clock.Advance`, `Scrub`, mid-run `SetVolume` and `node drain`; every 50
steps heal + quiesce and assert I-4, I-9, I-11, I-12, I-13; `-seed`/`-replay` reproduce.
With docs/16 node 0 carries a mirror: after every quiescence the checker begins a snapshot
and keeps the world as of its index, runs the copy some steps later as the host's worker
would while the volume moves on, takes the mirror offline and back and runs GC (held while
the sync runs); a sync whose host restarted or whose mirror is away ends failed. At every
quiescence every complete snapshot reads byte-exact against its world through every
gateway, no chunk a sync could not read was collected while held, and the catalogue agrees
on every node with the rest of the state (I-179, I-181, I-185). Thinning is the host's
schedule, covered by the `TestSchedule_*` tests.

**End to end** — `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes` (in `-short`, ≈5 s).
`test/mount/TestMount_Workload`, gated on `TAILRAID_MOUNT_TEST=1` and `sudo -n true`: real
`mount -t nfs`, then `cp -r` + `diff -r`, `dd … seek=N conv=notrunc` + `cmp`,
write-tmp-rename ×100, `truncate -s`, concurrent reads during writes, umount, remount,
`diff -r`.

**`internal/webui`** — handler tests on `testharness.Fabric` with a real `WhoIsAuthorizer`
over `tailscale.Fake`: authorization table (member, same-user non-member, other user,
tagged, unresolved own login, WhoIs down) and a body-unread 403; the CSRF header; a path
table (`..`, `//`, backslash, root, directory, NUL, missing); Range boundaries
{0, 1, size−1, size, size+1} plus a chunk-crossing range on a file written at a non-zero
offset, ETag / 304 / HEAD, read-your-writes on a dirty overlay, content types and the
sandbox headers; upload size table {0, 1, CS−1, CS, CS+1}, overwrite-truncates, the
WAL-cap wait, mkdir, stale gateway → 503; the status cache with the fake clock; the
embedded assets. See docs/13 §Tests.

## 5. Speed and determinism

- `go test -short -race ./...` ≈ 2 min on the dev laptop (`make check`). Measured caps:
  ec, chunk, meta, rpc, admin < 10 s; vfs < 12 s (WAL fsyncs); nfsgw < 8 s; cluster < 20 s;
  test/cluster < 90 s. The last one is dominated by Raft elections after kills: the harness
  runs 1 s heartbeat/election timeouts because shorter ones churned leadership under
  `-race` load, so every kill or bootstrap costs one to two seconds. Short runs cut the
  oracle op counts and iteration counts, never the fault coverage; `make test-long` runs
  the full counts.
- Behind `-short`: the model checker, long durability iterations, the 50k-op oracle, fuzz
  beyond seeds, the real-mount workload, every `Durable()` cluster test except one smoke.
- `make test-long` also records which lifecycle cells (`L-n`) the model checker hit; the
  soak report shows the same coverage table.
- No `time.Sleep` for synchronization in tests (forbidigo, `//nolint` only in the raft
  timeout helper with a comment). Waiting is `Eventually(cond, 5s, 1ms)` or channels; Raft
  apply completion is observed via `ApplyFuture.Error()` and `Cluster.Quiesce`.
- Every randomized test takes `-seed`, defaults to time-based, prints the seed on failure.
- `goleak.VerifyTestMain` in every package that starts goroutines; the ignore list is empty
  unless an entry carries an upstream issue link.
- `t.Parallel()` only in pure packages (ec, placement, meta).
- **Hot-path benchmarks** (`make bench`): flush of full chunks, cached and uncached reads,
  Commit on a 16k-chunk file, LOOKUP / GETATTR / READDIR on a 10k-entry directory, FSSTAT
  over 100k chunks, the chunk walk, shard-store PUT, cache GET, extent insert and cover
  checks, encode and decode. A baseline is committed as `docs/reports/bench-<date>.txt` and the run after a
  round of work as `bench-<date>-after.txt`; a performance change carries a `benchstat`
  delta against the baseline in the commit message.

## 6. Soak suite and live verification

`internal/soak` runs one scenario sequence against two environments through one interface
and renders one report. The driver never touches a kernel NFS client: Docker Desktop's VM
has none, and a live run already has the OS mount.

```go
type Env interface {
    Name() string; Nodes(ctx) ([]NodeRef, error)   // name, idx, RPC address
    Volume(node NodeRef) (Volume, bool)            // file IO through that node's gateway
    Caps() Caps                                    // Kill, Partition, Corrupt, MultiGateway, Disks, Mutate, Mirror
    Wait(ctx, d time.Duration) error               // real time, or the fake clock in tests
    // admin surface (one node's view; leader-only ops routed by the Env)
    Status, RepairNow, Scrub, GCNow, SetPolicy, SetPolicyAuto, SetState, Remove, Fsck,
    Chunks, FSMHash, Goroutines, Disks, Fill, S3 ...
    PurgeTrash(ctx) (int, error)                   // `trash purge --all`: what phase 8 needs before GC can reclaim
    WebURL(ctx, node NodeRef) (string, error)      // the node's browser UI (docs/13), for phase 16; "" skips it
    WebClient() *http.Client                       // how the driver reaches it (nil: directly); compose dials its published ports
    AddMirror(ctx) (name string, host NodeRef, err error)  // phase 19's scratch mirror (`mirror add`); "" on a live cluster
    SetMirrorOnline(ctx, name string, online bool) error   // unplug it and plug it back in
    SnapshotOp(ctx, node NodeRef, op rpc.AdminOp) (json.RawMessage, error)  // snapshot_ls, snapshot_create, versions, versions_revert, snapshot_restore
    // faults
    Kill, Start, Restart, Partition, Heal, Corrupt, FailDisk, RestoreDisk, AddDisk ...
}
type Volume interface { Mkdir, WriteAt, ReadAll, Hash, Truncate, Rename, Remove, Size, List }  // Hash streams sha256 + size
```

(`internal/soak/env.go` is the definition; the sketch names the groups.)

- **Compose** (`cmd/soak`, `test/soak/docker-compose.yml`): four `tailraid` containers
  (n1–n3 voters, n4 non-voter) with `dev.enabled`, `repair.grace` and `repair.fill_after`
  of 20 s, and one **worker** sidecar per node sharing the node's network namespace
  (`soak-client worker`). The worker reaches the node's loopback NFS gateway through the
  pure-Go NFSv3 client (with a 60 s call timeout and a fresh connection after any
  operation that overruns 75 s: a reply landing after that client's own timeout wedges its
  receive loop for good, and a follower gateway legitimately holds a metadata operation
  for up to the 30 s Stale threshold during an election) and the node's RPC from the node's own tailnet IP (so WhoIs sees a
  member); it exposes both over a small HTTP API on a published port. The driver runs on the
  host: `Volume` and `Call` go through the worker, kills, restarts and partitions through
  `docker compose kill|start|restart` and `docker network disconnect|connect`, corruption
  through `/dev/corrupt`.
- **Live** (`tailraid verify [--report out.html] [--destructive]`): runs on one device of
  the real cluster. `Volume` is the device's own mount path (kernel NFS client), `Call`
  uses the device's tailnet IP. Non-destructive by default: transfer, integrity across every
  node's FSM hash, churn, dedup and GC, repair backlog; the only thing a non-destructive
  run changes is the files under its own directory (I-113). `--destructive` adds corruption
  (`/dev/corrupt`, needs `dev.enabled` on the targets) and the steps that change the
  cluster itself (`Caps.Mutate`: policy changes, drains and removals, a disk fill);
  `--destructive --restarts` declares
  that every target runs under a restarting service manager (docs/12 §1) and adds kills,
  restarts and partitions through `/dev/exit` and `/dev/fault`; without it those phases
  are reported as *skipped (capability)*. All IO is confined to `/_soak/<timestamp>/`, deleted
  at the end. `--full` runs the long profile (the same one as `make soak-full`: a 64 MiB
  corpus file, the 1 GiB large file, more small files and churn, ten leader kills; about
  an hour) instead of the quick one.
- **In-process** (`internal/soak` tests): the same phases run against the `testharness`
  cluster with a billy-backed `Volume`, the fake clock behind `Wait`, and tiny sizes, so the
  scenario code and the report are unit-tested in seconds.
- **Phases** (quick profile in parentheses): 1 baseline transfer (corpus of 0 B … 3 MiB,
  sparse, 150 small files; throughput and latency histograms); 2 cross-view integrity
  (every reachable gateway reads the manifest, `fsck`, FSM hashes agree at one applied
  index); 3 random-access churn (writes at random offsets, truncates, renames through two
  gateways on disjoint files against a model; 20 s); 4 parity (kill m: reads and writes
  continue, `degraded` drains to 0 after `fill_after`; kill m+1: every read is either exact
  or an error, never garbage); 5 corruption (m shards: read exact, scrub repairs; m+1:
  error, never garbage, then the lost file is deleted and purged from the trash, as an
  operator would, so its unrepairable chunk leaves every later count); 6 consensus (kill the leader under writes ×3; acknowledged writes
  persist; FSM hashes agree); 7 membership (`set-redundancy` 2+1→1+2→2+1 under load with
  `stale_policy_chunks` draining to 0; drain and re-activate n4); 8 dedup and GC (second
  copy of the corpus adds < 1 % stored bytes; removing a unique file leaves its bytes
  stored and the entry in the trash, purging it (`trash purge --all`) tombstones its
  chunks, and GC reclaims them after grace, `fsck` clean); 9 restart storm (restart every node with dirty WALs; integrity); 10
  partition (a minority is cut off: the majority elects and keeps committing, a cut-off
  gateway refuses metadata operations and buffers WRITEs that commit after the heal; then
  the gateway itself is cut off and rejoins; a refused operation may land after the heal,
  which Raft allows, so the check is FSM agreement, not absence; I-29, L-59, L-148); 11
  automation (every budget within the hysteresis of `/dev/disk`, a 2 GiB `/dev/fill` on
  one node moves its store cap within two ticks, the corpus's data slots sit on voters,
  `set-redundancy --auto` moves the policy to the member table's rule with the migration
  complete and the corpus exact, then the previous pin is restored; I-74, I-75, I-77); 12
  large file (a file many times the WAL cap, 256 MiB in the quick profile and 1 GiB in
  full, written through the gateway in 1 MiB pieces and hashed as it goes, so the driver
  never holds it: every write acknowledged with no `EIO` while the client outruns the
  flusher, the WAL never past its cap plus one record, flush buffers within the I-78
  formula and the process heap flat, the content hash exact, and afterwards an empty
  `staging/`, no temp file, a clean `fsck` and every new orphan explained (the phase
  first runs repair passes until one finds nothing to do, so a policy migration a
  previous phase's restore started does not leave moved chunks' old shards under it; the
  write path may still leave a few for the sweep, L-190, so each orphan fsck lists after
  the phase and not before passes only when its chunk's history on some node records a
  PUT that failed or was cut, an upload whose commit did not apply, a repair move or a GC
  collection, and the check names every orphan nothing explains; until 2026-10-13 it
  demanded none at all and failed on two such shards); then where the file's
  chunks landed: every chunk fully placed, its data slots on voters, data slots spread
  among the voters in proportion to capacity within the statistical margin, and a
  per-node figure of its data and parity slots against each node's capacity share;
  I-78, I-79, I-80, I-38, I-9, I-33, I-77); 13 disks (every chunk within the host cap and no
  disk holding two slots; drain all hosts but one so their chunks pile onto its disks with
  no skew reported, re-activate them and watch `domain_skew` rise and drain to 0; close
  one disk by moving its marker and restarting its host: reads stay exact, the disk is
  dead while its host is live, repair re-places its slots within the cap after
  `fill_after`; restore it: live again, its stale shards swept after grace; then, in auto
  mode, add a disk to the gateway host (the policy migration lands every chunk at home,
  `volume rebalance` opens the gate for what is left and the passes move it onto the disk
  until `movable_bytes` and `disk_skew` are 0, a further pass moves nothing and the corpus
  is exact), drain it and remove it, the policy following the
  member table after each step with the corpus exact (the previous pin restored after);
  I-86, I-87, I-89, I-90, I-172, I-294, I-295, L-192, L-193, L-194, L-211); 14 diskless host (a host with no disks is live, flagged
  `diskless` with no budget and holds no slot; a corpus written through it is fully placed
  with no slot on it and reads exact through it and through another gateway; skipped on a
  rig where every host has a disk; I-118); 15 s3 bucket (the rig's bucket registered with
  `node add-s3` and its key in the secrets store (docs/19 §6), live on every gateway
  within the S3 probe interval, a second registration of it refused; a corpus written
  while it is a target lands on it in the disk layout and framing and reads exact through
  every gateway; with the host of another slot killed, a chunk that needs the bucket
  reads exact at once, before any probe has noticed, and again after the liveness
  threshold; an object deleted behind the cluster's back is found by the scrub and put
  back; GC collects a file on the bucket and the drain moves every slot off it, neither
  deleting an object; the drained bucket is removed and the corpus still reads exact. On a
  live cluster the phase uses the cluster's own S3 node when this device holds its key:
  it registers nothing, skips the refusal and the drain, deletes behind the cluster's
  back only with `--destructive`, and is skipped where there is no S3 node or no key; I-52,
  I-137, I-138, I-42, I-149, L-104, L-112, L-139); 16 web ui (the browser UI of docs/13
  driven over HTTP at the node's `web_url` as a same-user device: on the compose rig every
  node's fake tailnet gives the nodes and the driver's address, the bridge's gateway, one
  login, so the driver is a non-member device of the nodes' user (docs/13 §2); listings and stats
  against the corpus model; rename, move and delete batches with a deliberate taken
  name and a protected entry inside, every item reported on its own and the untouched
  ones unchanged; the trash round trip: a delete listed in place with its dates and its
  bytes readable with `in=`, the share seeing a delete (the mount's listing omits it, a
  read fails, a new file takes the name), an `rm -rf` through the mount restored with
  one call (read back once the mount's attribute cache lets the restored names show,
  within 90 s on a kernel NFS client, docs/07), a dirty file deleted at once and
  restored with every acknowledged byte, an
  overwrite through the web and through the mount whose previous version reads back from
  the trash, `.DS_Store` and an upload temp deleted on the mount and absent from
  `/api/trash`, a restore onto a taken name refused then done under a new name, a purge
  and `trash purge --older-than 0` through the admin RPC followed by GC's reclaim after
  grace; the flags: `protected` set on a folder, a web delete, a move out and a rename of
  it refused with 403 and the mount's remove with `EPERM` while an upload into it and a
  move into it succeed, `nsfw` effective on a nested file's stat (the flag changes need
  `Caps.Mutate`, else those checks are skipped); thumbnails: a JPEG `PUT` for a
  `.mp4`-named file read back byte-exact and a generated image thumbnail decoding to at
  most 320 px wide, `dir.cover` naming the tagged file, any user operation on
  `/.tailraid` refused with 400; tags: a file with an ID3v2 tag and a picture written
  through the volume, `/api/tags` returning its fields and `/api/thumb` its picture; the
  integrity panel's pages of the large file, every report with a `slots` array and the
  hashes equal to `Env.Chunks` over RPC; and pins: a folder pinned until
  `cache.pinned_bytes` reaches its bytes, then with `Caps.Kill` the other holders of its
  chunks stopped and the folder read through the web, then unpinned; everything the
  phase changed restored on its way out; I-152..I-165, L-199..L-208); 17 hot path (a 64-chunk file written
  through one gateway and read whole, cold, through another: the content exact, the
  cold-read MiB/s reported, readahead having served every chunk but the first window
  (`readahead_joined`, docs/12 §8), fetched no more than the file's chunks, and never more
  than `ReadAhead` prefetches in flight in any status sample; on a rig with one gateway the
  readahead checks are skipped when the client's own cache served the read; then, where
  the environment can partition, the writing gateway is cut off from one holder a quarter
  of the way through a second file: the gateway marks the holder suspect within the
  straggler grace and before the prober's threshold, the chunks committed after the cut
  hold no slot on it, the mark clears on the first good probe after the heal, repair fills
  the degraded chunks and the file reads back whole; I-166, I-169, I-170); 18 repair fan-out (a
  64-chunk file written and read cold through another gateway for an idle figure, then
  the neighbouring policy set so every chunk is stale, the migration pass run while the
  leader's status is sampled every 100 ms and the same file read cold again: the pass
  rate, the in-flight peak and both reads reported, repairs in flight never above
  `repair.workers`, the pass having overlapped chunks, the read during the pass keeping
  at least a third of its idle throughput, then the policy restored and the migration
  back complete; the in-flight and during-the-pass checks are skipped when the pass
  ended before a sample saw it; needs `--destructive` on a live cluster; I-174, I-175); 19
  snapshots (docs/16; the compose rig lists `/mirrors/m1`, a volume of n2's own, as a
  mirror with a one-minute schedule, `Caps.Mirror`; needs `Caps.Mutate`, and restores
  what it changes: the phase's files are written and committed, then the mirror
  registered with `tailraid mirror add` and online on its host within a tick, and a
  manual snapshot synced (after the schedule's own first sync, which copied the volume:
  its `new_bytes` are at least the phase's files); then one file overwritten in place
  through the mount, another renamed, a folder deleted and purged from the trash, a
  fourth truncated and a new one written, and GC run through the admin RPC after grace
  until the overwritten chunks are gone from the pool; every file of the first snapshot
  read through another gateway under `/.snapshots/<mirror>/<name>/` byte-exact against
  the model (the overwritten one from the mirror), the new file absent, a write, a mkdir
  and a remove under it refused; a second snapshot whose `new_bytes`, with those of any
  automatic one in between, are the changed chunks and trees only, not the corpus; the
  overwritten file's versions (the `versions` admin op) two contents with the first held
  by the first snapshot, a revert of it reading back the snapshot's bytes, and the
  deleted folder restored by `snapshot_restore` under a new name and compared; the new
  file then deleted and collected, so only the mirror holds it; the mirror unplugged (its
  directory moved away): its host reports it offline within a tick, the catalogue still
  lists its snapshots, reads inside fail on its host and, for the file only the mirror
  holds, on another gateway, nothing is begun for it over a whole interval, and on its
  return an automatic catch-up snapshot begins within a tick and the file reads exact
  again; the rig's mirror is a scratch directory, so the phase's manual snapshots, which
  no user removes, stay on it (the thinning that removes automatic ones is proven by the
  schedule's tests); on a live cluster the phase takes no snapshot at all, since one it
  took could never be removed: it reads the newest complete snapshot through every
  gateway and checks a mkdir under it is refused, and is skipped where the cluster has
  none; the browser's as-of route (409 for an offline mirror) joins with docs/13's
  snapshot routes; the phase records one event per snapshot with its sizes and what it
  added, the mirror's offline stretch, and one `snapshot round trip` event; I-179..I-182,
  I-185, I-186, I-188); 20 stats (with `stats.every: 5s`
  on the rig and 1 s on the harness: after `stats seal` every node's raw segment is
  under its own directory, decodes and is below the small-chunk threshold and the
  leader's cluster segment is there, `stats seal --rollup` writes today's minute and
  coarse segments and the minute rows equal the pure rollup of the raw rows the driver
  read back, the leader's seal removes a segment the phase planted with a 2010 name,
  the page's charts render for a 6 h range from the rows read and the live edge every
  node answers over `/v1/stats`, a member killed mid-phase answers no live edge and the
  leader's rows mark it dead and never a zero, and the stored bytes grow by no more
  than docs/17 §6's estimate for one sealed hour and the rollups per node, printed
  measured against estimated; I-191..I-194, I-196); 21 search and
  media (text files with known words, a tagged track, a Kodi-style film folder and a show
  folder written through the mount, the libraries flagged, `index_now` over the admin RPC,
  then every node's `/api/search` and `/api/media` compared byte for byte, a renamed file
  found at once, a trashed one gone from both, a snippet range equal to the bytes, a
  markdown file rendered with only the renderer's tags, the film's plot and poster and the
  episode numbering; flags restored under `Caps.Mutate`; I-206..I-212); 22 copies under
  faults (written after the 2026-10-12 copies that left files of holes: each round starts
  copy jobs on two gateways that the round does not fault, one over SMB on one
  connection and one over NFS with one open handle per file, each file created, sized
  to its final length first and written front to back in 1 MiB writes the way Explorer
  copies, then injects one fault while they run: (a) every other node restarted in turn
  the way an update restarts it, graceful, each back before the next; (b) the leader
  killed and brought back; (c) a minority partitioned off and healed; (d) the leader's
  prober taking one member for codec 4 while the gateways' do not (`/dev/codec`), the
  disagreement of the 2026-10-12 upgrade, held through the round's checks; (e) every
  shard PUT delayed so the flush falls behind the copies; (f) a copying gateway
  restarted the way `update --restart` restarts it, which must wait for its copies to
  finish (`admin.WaitIdle`). Checks per round: every copy reported success with no
  client error (I-217, I-219, I-221); every copy reads back with its sha256 through
  another gateway (I-7); no copied file is all holes (the integrity panel's "zero"); the
  cluster is healthy; FSM hashes agree after the last round. Against a build before
  I-219, round (d) fails: every copy reports success and none reads back whole, the
  commits carrying shard sums refused for as long as the leader sees the old member.
  The stall of I-217 and the update of I-221 do not bite at the phase's file sizes and
  are pinned by their own tests. A failed copy's cleanup
  under a protected folder (docs/15 §8) is not driven here but in phase 23); 23 many
  users at once (written after research for the 2026-10-12 incident found that two
  clients meeting in one name could truncate each other's copy: every round is
  contention between clients rather than a fault, each in its own folder, and every
  file is read back through a gateway that wrote none of it, checked against its
  payload's sha256 and for holes as in phase 22): (a) many streams into one gateway,
  three SMB sessions of four files each and an NFS job at once (I-217, I-167);
  (b) two gateways copying one folder tree into the same new folder, both making the
  same subfolders, a third gateway's listing reaching the union (I-35); (c) 24 names
  each created exclusively by four clients, two SMB sessions of one gateway and an SMB
  and an NFS client of another, exactly one success per name, the others told the name
  exists, and the file its winner's (I-222; the race only bites a name whose losing
  gateway had not yet applied the winner's create, so this round's power to find the
  bug is a matter of timing, and `TestCluster_ExclCreate_TwoGateways_OneWins` is the
  deterministic test); (d) two names each replaced at once by two clients through two
  gateways with different payloads larger than a flush (16 chunks and 40 MiB, at most
  half the profile's large file), so each copy commits in several flushes that
  interleave with the other's, every gateway reading one payload whole, the same one,
  once both writers report nothing dirty, planned or in their WAL (until then each
  reads its own copy) (I-223); (e) a
  gateway reading a file another gateway is copying, every read the old bytes or the
  new chunk by chunk and never other bytes (I-36), the whole file once the copy has
  ended and `FlushIdle` passed; (f) the folder of SMB and NFS copies renamed through
  another gateway once every copy has made its file, every file whole under the new
  name (a delete under an open copy differs by protocol, an NFS handle going stale and
  an SMB one writing on into the trash entry, and is pinned by I-155's own tests); (g) on one gateway, a handle
  marked delete-on-close whose file another session renames away and replaces with a
  new file before the close, the new file surviving and the renamed one gone, a
  no-replace rename onto an existing name refused, and a rename seen by the other
  session's open handle (I-224); then across gateways, the case the inode check is for:
  a delete-on-close handle held open on one gateway (`RaceEnv.HoldDeleteOnClose`, the
  worker's `/smbhold` and `/smbrelease`) while another gateway renames its file away and
  puts a new one under the name, the new file surviving the close (I-225, the own
  cleanup of I-220 included); (h) two browser uploads of one name through two gateways, plain and
  with `If-None-Match: *` (I-226); (i) round (a) again while the leader is killed. After
  the last round the FSM hashes agree. 24 secrets (docs/19 §7): a secret set through one node reads back through
  every node, every node lists it with no value in the reply, a replace through another
  node reads back through the first, a node whose keychain copy is wiped fetches the key,
  the others read it with its setter killed, and a removal reaches every node
  (I-303..I-307). Phases 25–27 are unused (the mesh of 0.9.x). 28 users and keys, 29 grants on the gateways, 30 secrets per user and 31 audit
  and sessions are docs/21 §10: an invite minted on one node creating a user on another,
  a second device's key, a disabled user and the last admin's lockout; read, write,
  admin and nobody on NFS and the web with the same answer from two gateways, a revoke
  refusing the next write; personal secrets isolated per user and the system space
  admins-only; one audit record per operation, refusals counted, a browser session
  minted on one node, handed off to another's page and ended from there (I-342..I-358).
  The driver holds the people as Ed25519 identities it makes for the run and signs every
  operation with; the first admin comes from `tailraid user bootstrap` in a node's
  container, the compose worker relays a signed request (`rpcReq.Sig`), and the driver's
  mounts stay bound to the admin between the checks. The phases run last, since the
  first user turns enforcement on for the rest of the run; a live cluster's users are
  its own, so it records the four as skipped (`Caps.Users`); the harness answers the
  same ops over its nodes' own FSMs (`users_harness_test.go`), so CI runs the four.
  **Users first** (`make soak SOAK_FLAGS=-users-first`, `Run.UsersFirst`): a real volume
  has its first admin from `init`, so a run can put phase 0 before phase 1: the admin
  made as phase 28 makes them, the driver's mount uid and SMB account bound to them on
  every node, the environment told to act as them (`UsersEnv.ActAs`: the compose rig
  signs every mutating RPC, the harness runs its own trash purge, snapshot restores and
  secrets as the admin's user) and the driver's browser signed in on every page through
  `web login`, signing in again on a 401. Every phase then runs with the FSM checking
  every command's actor, and phase 28 reuses the admin (I-362). `/.tailraid` is no user's
  once the cluster has users (docs/21 §3), so phase 20 reads and plants its segments
  through the environment's own reading of it (`ReservedEnv`, the harness's node
  filesystem) and records itself skipped where there is none. Without users first, every
  phase but 28–31 ran on a cluster with no users, where nothing is checked: an admin's
  `trash purge` and `snapshot restore` were refused on the real cluster for want of an
  actor while every soak stayed green (2026-10-15). The harness's admin shortcuts do not
  run `node.adminOps`, so the signatures and the needs table of every op but 28–31's are
  exercised under users by the compose run alone. `soak.CopySpec`
  carries the open mode (exclusive, the default, or replace) and
  which of the job's sessions copies it (`Conn`), and a result says when a create lost
  to an existing name (`Exists`); the handle races are `smbclient.Races`
  (`internal/soak/smbclient`, kept out of the tailraid binary) behind the optional
  `RaceEnv`, run by the compose worker's `/smbraces` and by the harness against one
  SMB gateway per node; the SMB side uses `third_party/smb2client`, the
  in-tree copy of `cloudsoda/go-smb2` with the two calls upstream lacks (mark an open
  handle delete-on-close, rename without replace). Phases 22 and 23 need copy jobs, which
  only the compose rig and the harness run: `tailraid verify` on a live cluster records
  both as skipped. A round that faults the cluster registers the fault's end with the
  phase's undo, so a phase cut short still brings the leader back or heals the partition
  (I-112); a copy job the worker reported ended is forgotten, so the poll that saw the end
  keeps its results, and the harness's jobs behave the same. The compose rig gives four nodes two disks each, one
  derived and one pinned, plus a fifth diskless voter (4 × 2 targets and a gateway-only
  host; phase 14 writes through it), an `s3` service, rclone's S3 server with
  signature checking on, as phase 15's bucket, a mirror directory on n2 for phase 19,
  and publishes each node's web port for phase 16; `cmd/soak -phases 15` runs a subset of
  the phases on the rig; `cmd/soak -topology single` runs one node with four
  disks, where phase 13 checks the automatic 2+2 and the disk failure. The sampler also records each sampled node's
  machine vitals (docs/12 §8) and the report charts CPU, memory, memory pressure, disk
  and network (the tailnet interface's rx and tx, I-190) next to the process memory. A phase the environment cannot run is recorded as
  *skipped (capability)*, never passed.
  On a live cluster a kill is held down: the victim's peers drop it, `/dev/exit` bounces
  it, and once the service manager has it back it drops every peer until the phase's
  `Start`, so "m nodes dead" holds for as long as the phase needs rather than for the
  service manager's restart delay. No rule ever names the device running `verify`: its
  calls must still reach a held-down or cut-off node to lift the rules, and its own
  node's rules cut the link in both directions on their own (the three-VM rounds found
  a partition that could never be healed otherwise). The device running `verify` is never killed (its mount
  is the run's only volume); when it leads, the consensus phase is skipped.
- **Driver rules.** A phase runs in its own goroutine under the profile's `PhaseTimeout`
  (20 min in the quick profile; 0 is no limit): a phase that panics or overruns is one
  failed "phase completed" check with the driver's stack or every node's goroutine dump
  behind it, and the run continues with the next phase (I-111). A phase that recorded no
  check and did not skip fails too: silence is not a pass. A check that could not be
  measured (no sample landed, the gateway reports no WAL cap, the file is too small to
  judge a spread, a victim never answered) is recorded `skipped` with its reason, counts
  as neither pass nor fail and covers nothing (I-114). Every phase that kills, cuts,
  drains, fills or re-pins registers the restore with an `undo` list and runs whatever it
  did not get to on its way out, on a context that outlives a cancellation, so a failed,
  timed-out or interrupted phase leaves the cluster as it found it (I-112). `Execute`
  always returns a summary: a run that stops before its first phase (no node, no status,
  no gateway, the run directory refused) or is cancelled between phases carries
  `Aborted` and the phases it never reached as *NOT RUN*, the report renders with an
  `ABORTED` banner, and `verify` and `cmd/soak` write that partial report and exit
  non-zero; Ctrl-C on `verify` ends the run after the current phase and the cleanup of
  `/_soak/<ts>` still runs. The goroutine dumps of a run are capped at 4 MiB altogether.
- **Results and report**: the driver appends one JSON line per event, check and 1 s
  sample (bytes moved, cumulative write/read latency histograms over fixed log-spaced
  buckets, degraded / repair-pending / unreadable / stale-policy chunks, stored and
  logical bytes, dedup ratio, slots and liveness per node, the sampled node's overlay WAL
  depth, FSM-hash agreement, the trash's inodes and bytes, the sampled node's pinned
  cache bytes, the catalogue's snapshot count and each mirror's used bytes) to
  `results.jsonl`; every record carries its line number.
  Every check cites the invariants and lifecycle cells it exercises (`I-n`, `L-n`; a
  test keeps the citations defined). The driver then renders a self-contained
  `report.html` with inline SVG charts drawn in Go: the header (target, profile, policy
  and chunk size, node count, seed, flags, per-node binary version), a PASS / FAIL /
  PARTIAL / ABORTED banner (PARTIAL when a phase was skipped for a capability the
  environment lacks: a live run without `--destructive --restarts` never shows green;
  ABORTED with the reason when the run stopped early), the phase ×
  check matrix with citations (`ok`, `FAIL` or `SKIPPED` per check), every inconsistency with file, chunk, node and its
  `results.jsonl` line, totals (bytes, ops, dedup ratio, deepest WAL, repair and GC
  counters), p50 / p99 / max latency per phase and separately for the operations issued
  while a fault was in force, the coverage table of cited IDs with pass and fail counts,
  the notes the phases left (an FSM-hash check that could not run, a refused operation
  that landed after a heal), every node's goroutine dump taken right after a failed
  phase (`/dev/goroutines`; the compose driver appends the worker's own dump), and the charts: throughput by sample interval with phase
  bands and every event marker, node liveness lanes, repair backlog, stored vs logical
  bytes, overlay WAL depth, slots per node, network throughput, latency histograms, the
  FSM-hash strip, the trash and the pinned cache over time, the snapshot count and each mirror's used bytes over time with a bar per snapshot of what it added to its mirror and the phase's round-trip table (drawn only when a snapshot was taken; the lookdev is `assets/lookdev/soak-snapshots.html`), and the data spread across disks (one lane per
  target grouped by host, each sample coloured by the target's slot share against its
  expected share, dead hatched, with a host-cap strip underneath that is green while no
  chunk exceeds the cap and amber while `domain_skew > 0`).
- `make soak` (quick, ~10 min) runs the compose profile; `make soak-full` the long one.
  Setup (two cross-compiled binaries, eight cached image layers, four joins) takes about a
  minute on an idle machine; run it on its own, since `make check` alongside it turns that
  into ten.
  Acceptance for M3b includes injecting one deliberate bug and watching the suite go red.
- **Three local VMs** ([vms.md](vms.md)): one macOS, one Linux and one Windows guest on
  this Mac, joined to a throwaway headscale, running the real binaries under launchd,
  systemd and the SCM with the real NFS and SMB mounts. `make vms-rounds` runs three
  rounds, each OS running `init` in turn while the other two join by name, restarts the
  joiners, moves data across the three mounts, runs `verify --destructive --restarts` from
  the first node and recovers a one-voter cluster. It is the tier where Tailscale, WhoIs,
  the service managers and the kernel mount clients are real, and the one that would have
  caught the v0.3.2 join defects (I-76, I-92).

## 7. CI

`.github/workflows/ci.yml`: `checks` (`go mod verify`, tidy diff, `make vendor-check`:
the committed `vendor/` matches `go.mod`, gofmt) · `lint` (golangci-lint v2 via
`go run -mod=readonly -modfile=tools.mod`, `check-invariants.sh --check`, `make deadcode`,
`make web-check`: the committed browser bundle matches `internal/webui/src`; the lint runs
again under `GOOS=linux`, `GOOS=darwin` and `GOOS=windows` with `make lint-cross`, so the per-OS files are covered on every developer machine and in CI) · `vuln`
(govulncheck; the release job waits for it) · `test`
matrix ubuntu/macos/windows `go test -short -race -cover ./...` (no coverage gate) · `long`
on pushes to main and nightly (`make test-long`) · `mount` on main (ubuntu with
`nfs-common`, macos; `sudo -E make test-mount`) · `soak` nightly on ubuntu uploading
`report.html` · `release` on tags `v*` (6 static binaries + sha256sums). Go version is read
from `go.mod`.

## Invariants

The soak driver's own rules (§6 "Driver rules"); the phases cite the invariants of the
components they exercise.

- **I-111** — A phase that panics or runs past `PhaseTimeout` is recorded as one failed
  check of that phase, with the driver's stack or every node's goroutine dump as a dump
  record, and the run continues with the next phase; a phase that records no check and
  does not skip is failed as well.
- **I-270** — A phase's first failed check saves the rig's evidence, once per phase,
  into `<evidence>/<phase>/` (the driver's `-evidence`, beside the report by default) and
  the results name the folder; on the compose rig that is `docker compose ps` and, for
  every node, its last 2,000 log lines with its worker's and its `tailraid status --json`.
  Before, `make soak` tore the rig down with its logs, and two failures of 2026-10-13
  (a restarted gateway serving an empty tree, two orphans in phase 12) stayed
  unexplained.
- **I-112** — Every change a phase makes to the cluster (a killed node, a cut, a drained
  or removed member, a filled disk, a policy change) is restored before the phase's result
  is recorded, whether the phase completed, failed, panicked, timed out or was cancelled,
  on a context that outlives the cancellation.
- **I-113** — A live run without `--destructive` changes nothing but the files under its
  own run directory: no policy change, drain, removal, fill, corruption, kill or cut is
  issued, and each such step is recorded skipped with its reason.
- **I-362** — A run with users first makes the first admin before phase 1 and binds the
  driver's mount uid and SMB account to them on every node; from then on the
  environment signs every admin call as that admin, its own shortcuts that write the
  volume act as the admin's user, and the driver's browser carries a session on every
  page; every phase passes as it does on a cluster without users, phase 28 reusing the
  admin, and a proposal of the CLI's or the node's own that names no actor fails a phase.
- **I-114** — A check that could not be measured is recorded skipped with its reason and
  counts as neither pass nor fail; a run that stops before or between phases returns a
  summary with the reason and the unreached phases as not run, and the report renders it
  with an `ABORTED` verdict, never `PASS`.

## Tests

| Invariant | Tests |
|---|---|
| I-270 | `TestExecute_FailureSavesEvidence` |
| I-111 | `TestExecute_PhasePanicIsFailure`, `TestExecute_PhaseTimeoutIsFailure`, `TestExecute_SilentPhaseFails` |
| I-112 | `TestUndo_RunsRemainingStepsInReverse`, `TestExecute_CancelledPhaseRestores`, `TestPhases_MembershipRestoresAutoMode` |
| I-113 | `TestLiveEnv_CapsFollowFlags`, `TestPhases_MutatingStepsGated` |
| I-362 | `TestSoak_AllPhasesUnderUsersAgainstHarness` |
| I-114 | `TestExecute_AbortBeforePhases`, `TestExecute_CancelMarksNotRun`, `TestRender_ZeroPhasesAborted`, `TestRender_NotRunPhases`, `TestSummary_SkippedCheckCoversNothing` |
