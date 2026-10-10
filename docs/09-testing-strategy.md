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
    Nodes(ctx) ([]NodeRef, error)                  // name, idx, RPC address
    Volume(node NodeRef) (Volume, bool)            // file IO through that node's gateway
    Call(ctx, node, method, path string, body []byte) ([]byte, error) // node RPC as a member
    Kill(ctx, node) error; Start(ctx, node) error; Restart(ctx, node) error
    Partition(ctx, a, b []NodeRef) error; Heal(ctx) error
    Wait(ctx, d time.Duration) error               // real time, or the fake clock in tests
    Caps() Caps                                    // Kill, Partition, Corrupt, MultiGateway
}
type Volume interface { Mkdir, WriteAt, ReadAll, Hash, Truncate, Rename, Remove, Size }  // Hash streams sha256 + size
```

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
  error, never garbage); 6 consensus (kill the leader under writes ×3; acknowledged writes
  persist; FSM hashes agree); 7 membership (`set-redundancy` 2+1→1+2→2+1 under load with
  `stale_policy_chunks` draining to 0; drain and re-activate n4); 8 dedup and GC (second
  copy of the corpus adds < 1 % stored bytes; removing a unique file reclaims after grace,
  `fsck` clean); 9 restart storm (restart every node with dirty WALs; integrity); 10
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
  `staging/`, no temp file, a clean `fsck` and no new orphan; then where the file's
  chunks landed: every chunk fully placed, its data slots on voters, data slots spread
  among the voters in proportion to capacity within the statistical margin, and a
  per-node figure of its data and parity slots against each node's capacity share;
  I-78, I-79, I-80, I-38, I-9, I-33, I-77); 13 disks (every chunk within the host cap and no
  disk holding two slots; drain all hosts but one so their chunks pile onto its disks with
  no skew reported, re-activate them and watch `domain_skew` rise and drain to 0; close
  one disk by moving its marker and restarting its host: reads stay exact, the disk is
  dead while its host is live, repair re-places its slots within the cap after
  `fill_after`; restore it: live again, its stale shards swept after grace; then, in auto
  mode, add a disk to the gateway host (repair passes move slots onto it until the host's
  disks are within the skew of their shares and `disk_skew` is 0, the corpus exact), drain
  it and remove it, the policy following the member table after each step with the corpus
  exact (the previous pin restored after); I-86, I-87, I-89, I-90, I-172, L-192, L-193,
  L-194); 14 diskless host (a host with no disks is live, flagged
  `diskless` with no budget and holds no slot; a corpus written through it is fully placed
  with no slot on it and reads exact through it and through another gateway; skipped on a
  rig where every host has a disk; I-118); 15 s3 bucket (the rig's bucket registered with
  `node add-s3` and every other node given its key with `node keys`, live on every gateway
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
  I-137, I-138, I-42, I-149, L-104, L-112, L-139); 17 hot path (a 64-chunk file written
  through one gateway and read whole, cold, through another: the content exact, the
  cold-read MiB/s reported, readahead having served every chunk but the first window
  (`readahead_joined`, docs/12 §8), fetched no more than the file's chunks, and never more
  than `ReadAhead` prefetches in flight in any status sample; on a rig with one gateway the
  readahead checks are skipped when the client's own cache served the read; then, where
  the environment can partition, the writing gateway is cut off from one holder a quarter
  of the way through a second file: the gateway marks the holder suspect within the
  straggler grace and before the prober's threshold, the chunks committed after the cut
  hold no slot on it, the mark clears on the first good probe after the heal, repair fills
  the degraded chunks and the file reads back whole; I-166, I-169, I-170). The compose rig gives four nodes two disks each, one
  derived and one pinned, plus a fifth diskless voter (4 × 2 targets and a gateway-only
  host; phase 14 writes through it) and an `s3` service, rclone's S3 server with
  signature checking on, as phase 15's bucket; `cmd/soak -phases 15` runs a subset of
  the phases on the rig; `cmd/soak -topology single` runs one node with four
  disks, where phase 13 checks the automatic 2+2 and the disk failure. The sampler also records each sampled node's
  machine vitals (docs/12 §8) and the report charts CPU, memory, memory pressure and
  disk next to the process memory. A phase the environment cannot run is recorded as
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
  depth, FSM-hash agreement) to `results.jsonl`; every record carries its line number.
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
  bytes, overlay WAL depth, slots per node, latency histograms, the FSM-hash strip, and the data spread across disks (one lane per
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
- **I-112** — Every change a phase makes to the cluster (a killed node, a cut, a drained
  or removed member, a filled disk, a policy change) is restored before the phase's result
  is recorded, whether the phase completed, failed, panicked, timed out or was cancelled,
  on a context that outlives the cancellation.
- **I-113** — A live run without `--destructive` changes nothing but the files under its
  own run directory: no policy change, drain, removal, fill, corruption, kill or cut is
  issued, and each such step is recorded skipped with its reason.
- **I-114** — A check that could not be measured is recorded skipped with its reason and
  counts as neither pass nor fail; a run that stops before or between phases returns a
  summary with the reason and the unreached phases as not run, and the report renders it
  with an `ABORTED` verdict, never `PASS`.

## Tests

| Invariant | Tests |
|---|---|
| I-111 | `TestExecute_PhasePanicIsFailure`, `TestExecute_PhaseTimeoutIsFailure`, `TestExecute_SilentPhaseFails` |
| I-112 | `TestUndo_RunsRemainingStepsInReverse`, `TestExecute_CancelledPhaseRestores`, `TestPhases_MembershipRestoresAutoMode` |
| I-113 | `TestLiveEnv_CapsFollowFlags`, `TestPhases_MutatingStepsGated` |
| I-114 | `TestExecute_AbortBeforePhases`, `TestExecute_CancelMarksNotRun`, `TestRender_ZeroPhasesAborted`, `TestRender_NotRunPhases`, `TestSummary_SkippedCheckCoversNothing` |
