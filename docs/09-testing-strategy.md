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
- `make check` (fmt-check, lint, invariants, `go test -short -race ./...`) runs in the
  lefthook pre-commit hook and must pass before every commit.
- No coverage gate. `make test` prints per-package coverage so regressions are visible.
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
| `sysfs.FS` (`CreateTemp, Rename, Remove, Open, ReadDir, SyncDir`; file `Write, Sync, Close`) + `sysfstest.Fault` | `internal/chunk` only | ENOSPC, torn writes, crash at each fsync step (`SyncDir` is on the ack path: a PUT is acked only after it) |
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
`TestStore_PutRejectsHashMismatch`; `TestStore_CrashBeforeRename_LeavesNoVisibleShard`;
`TestStore_CrashAfterRenameBeforeDirSync_NotAcked`; `TestStore_OpenSweepsTemps`;
`TestStore_TornWrite_DetectedOnRead`; `TestStore_ConcurrentPutSameHash` (32 goroutines,
`-race`); `TestStore_DiskFull_NoPartialShard`; `TestStore_DupPutDifferentPayload_Overwrites`;
`TestStore_DeleteIfOlder_KeepsYoung`; `TestCache_LRUEvictsBySize`; `FuzzStore_PutGet`.

**`internal/meta`** — `TestFSM_ApplySnapshotRestoreEquivalence` (5k random commands;
snapshot; restore; compare; also restore-then-replay-tail); `TestFSM_DeterministicAcrossReplicas`
(same log into 3 FSMs → byte-identical snapshots; catches map-iteration order);
rename table (`over file`, `over empty dir`, `over non-empty dir`, `into own subtree`,
`same path`, `cross-dir keeps ino+gen`); `TestFSM_HandleStableAcrossRestart`;
`TestFSM_InoGenNotReusedAfterUnlink`; `TestFSM_RefCount_SharedChunkAcrossFiles`;
`TestFSM_Commit_RejectsStaleBaseVersion`; `TestFSM_Commit_ErrNeedTailRewrite`;
`TestFSM_Commit_TruncateThenGrow_DropsOldChunks`; `TestFSM_NowIsMonotonic`;
`TestFSM_RefcountUnderflow_IsBadCommand`;
`TestFSM_SetVolume_NewPolicyAppliesToNewChunksOnly`; invariant checker used by every FSM test
(Σrefs = references from inodes, `len(Chunks) == ceil(Size/CS)`, parent chains reach root,
`Tombs ⇔ Refs==0`); `FuzzDecodeCommand`.

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
`TestVFS_WriteSpanningChunkBoundary`, `TestVFS_WritePastEOF_CreatesHole_ReadsZeros`,
`TestVFS_TruncateDown_DropsChunkRefs`, `TestVFS_TruncateUp_Zeros`,
`TestVFS_CloseFlushIsDurable`, `TestVFS_DedupIdenticalChunks`,
`TestVFS_TruncateThenGrow_OldBytesDoNotResurface`,
`TestVFS_ConcurrentGateways_DisjointChunksBothLand`, `TestFlusher_UnknownChunk_ReUpload`,
`TestFlusher_DegradedCommitRecordsZeroSlots`, `TestFlusher_ConflictRebase_KeepsAllExtents`,
`TestFlusher_ConflictRebase_PreservesConcurrentAppend`, `TestFlusher_PartialFlush_SizeNeverAheadOfData`,
`TestWAL_ReplayStopsAtTornTail`, `TestWAL_CompactKeepsLiveExtents`,
`TestVFS_UnlinkDirtyFile_DropsWAL`, `TestVFS_ConcurrentWritersDifferentFiles`.

**`internal/nfsgw`** — in-process over `willscott/go-nfs-client` against `nfs.Serve` on
`127.0.0.1:0` (no root): `TestNFS_WriteSequence_MultiRPC` (64 KiB and 1 MiB pieces, shuffled
offsets), `TestNFS_SetattrSizeTruncates`, `TestNFS_SetattrModeOwnerTimes`,
`TestNFS_RenameKeepsHandleValid`, `TestNFS_HandleSurvivesGatewayRestart`,
`TestNFS_RemovedHandleIsStale`, `TestNFS_BogusHandleIsStaleNotPanic`,
`TestNFS_ReadDirPlusLargeDirectory` (5k entries), `TestNFS_UnsupportedOpsReturnNotSupp`.

**`internal/rpc`** — `TestChunkRPC_SizeBoundaries` {0, 1, max−1, max, max+1 → 413} with client
and server limits asserted equal; `TestChunkRPC_HashMismatchRejected`; `TestChunkRPC_GetMissing404`;
`TestChunkRPC_ContextCancelAbortsUpload`; `TestRPC_DeleteFence_RefusesStaleIndex`,
`TestRPC_DeleteFence_RefusesYoungShard`, `TestRPC_DeleteFence_RefusesPlacedShard412`;
`TestWhoIs_DeniesUnknownPeer`,
`TestWhoIs_DeniesStableIDMismatch`, `TestWhoIs_AllowsMember`, `TestWhoIs_PurgeOnMembershipChange`;
`TestChunkRPC_ReadsAuthBeforeBody`; `TestRPC_ClusterIDMismatch409`;
`TestRPC_ListenRefusesNonLoopbackWithoutAuth`.

**`internal/cluster` + `internal/testharness`** — harness:

```go
type Cluster struct { Nodes []*Node; Net *FaultNet; Clock *clock.Fake }
func NewCluster(t *testing.T, n int, opts ...Option) *Cluster   // K(k,m), Durable(), Seed(int64)
func (c *Cluster) Leader(t *testing.T) *Node
func (c *Cluster) Kill(i int); func (c *Cluster) Restart(t *testing.T, i int)
func (c *Cluster) Partition(a, b []int); func (c *Cluster) Heal()
func (c *Cluster) Quiesce(t *testing.T)   // Barrier on leader + all FSMs at the same index
```

Scenarios: `TestCluster_WriteReadAllNodes`, `TestCluster_KillMNodes_AllDataReadable`,
`TestCluster_KillMPlusOne_ReadsFailCleanly`, `TestCluster_NodeDiesMidFlush_RepairRestoresRedundancy`,
`TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits`,
`TestCluster_RejoinAfterLongAbsence_SnapshotInstall`, `TestCluster_GCDoesNotDeleteInflightUpload`,
`TestCluster_SplitBrain_OnlyMajorityCommits`, `TestCluster_PlacementAgreesOnAllNodes`,
`TestIDStream_RejectsForeignCluster`, `TestCluster_TwoInitsDoNotMerge`,
`TestJoin_WrongToken403_NoRaftChange`, `TestCluster_IPChange_UpdatesAddr`,
`TestProber_LaptopSleep_NoRepairBeforeHoldDown`, `TestServe_SecondInstanceSameDirFails`.

**Data-loss specific** — `TestCrash_AtEveryPoint`: for each hook, start a write of a known
payload, kill the gateway at the hook, restart, and require exactly one of two outcomes:
not acknowledged ⇒ old content and no visible orphan shards, or acknowledged ⇒ new content
readable from every live gateway. `TestCluster_TornShardOnOneNode_ReadDecodes_AndRepairs`.
`TestDurability_AckImpliesReadableFromKSurvivors` (`Durable()`; after each ack: kill and
restart the gateway, let the WAL replay and flush, then kill m nodes and read elsewhere;
rotate the gateway; 20 iterations short / 200 long). **`TestModelCheck_Long`** (skipped under `-short`; `make
test-long`): 5 nodes 3+2, `Durable()`, a seeded scheduler of client ops, `Kill`, `Restart`,
`Partition`, `Heal`, `Clock.Advance`, `Scrub`, mid-run `SetVolume` and `node drain`; every 50
steps heal + quiesce and assert I-4, I-9, I-11, I-12, I-13; `-seed`/`-replay` reproduce.

**End to end** — `TestE2E_ThreeNodes_NFSWriteOnA_ReadOnB_KillC` (in `-short`, ≈5 s).
`test/mount/TestMount_Workload`, gated on `TAILRAID_MOUNT_TEST=1` and `sudo -n true`: real
`mount -t nfs`, then `cp -r` + `diff -r`, `dd … seek=N conv=notrunc` + `cmp`,
write-tmp-rename ×100, `truncate -s`, concurrent reads during writes, umount, remount,
`diff -r`.

## 5. Speed and determinism

- `go test -short -race ./...` ≤ 60 s on the dev laptop. Per-package caps: ec, placement,
  chunk, meta < 3 s; vfs < 8 s; nfsgw < 5 s; cluster < 25 s; e2e < 8 s.
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

## 6. Soak suite and live verification

`internal/soak` runs the same scenario sequence against two environments through one
interface and renders one report:

```go
type Env interface {
    Nodes(ctx) ([]NodeRef, error)
    Kill(ctx, node) error; Start(ctx, node) error
    Partition(ctx, a, b []NodeRef) error; Heal(ctx) error
    Corrupt(ctx, node, key) error
    Mount(ctx, node) (path string, cleanup func(), err error)
    Capabilities() Caps
}
```

- `composeEnv` (`cmd/soak`, `test/soak/docker-compose.yml`): 5 `tailraid` containers with
  `serve --dev`, a privileged client container with the kernel NFS client and rclone, a
  driver container. Kill/start via the Docker API, partitions via `docker network
  disconnect`, corruption via `/dev/corrupt`.
- `liveEnv` (`tailraid verify`): runs on one of the user's devices against the real cluster.
  Non-destructive by default; `--destructive` adds kill/partition/corrupt through `/dev/*`,
  which requires `dev.enabled` on the targets and a restarting service manager. All IO is
  confined to a fresh `/_soak/<timestamp>/` directory that is deleted at the end.
- Phases: baseline transfer; cross-view integrity (NFS on two gateways, S3, `fsck
  --verify-data`, FSM hash agreement, shard inventory vs chunk table); random-access churn;
  parity (kill m, then m+1); corruption (m, then m+1 shards); consensus (leader kills,
  partitions); membership (drain, remove, join, `set-redundancy` under load); dedup & GC;
  restart storm. Phases an environment cannot run are recorded as *skipped (capability)*.
- Report: one self-contained `report.html` from `results.jsonl` (1 s metric samples plus
  the event timeline) with inline SVG charts rendered in Go: pass/fail banner and invariant
  matrix, throughput with event markers, under-replicated and repair backlog over time,
  shard distribution per node, latency histograms, dedup ratio, FSM-hash agreement, and a
  table of every inconsistency with file, chunk, node and phase.
- `make soak` (quick, ~10 min) and `make soak-full` (~1 h). Acceptance for M3b includes
  injecting one deliberate bug and watching the suite go red.

## 7. CI

`.github/workflows/ci.yml`: `checks` (tidy diff, gofmt) · `lint` (golangci-lint v2 via
`go tool -modfile=tools.mod`, `check-invariants.sh --check`) · `vuln` (govulncheck) · `test`
matrix ubuntu/macos/windows `go test -short -race -cover ./...` (no coverage gate) · `long`
on pushes to main and nightly (`make test-long`) · `mount` on main (ubuntu with
`nfs-common`, macos; `sudo -E make test-mount`) · `soak` nightly on ubuntu uploading
`report.html` · `release` on tags `v*` (6 static binaries + sha256sums). Go version is read
from `go.mod`.
