package invariants

import "testing"

// Stubs for snapshots and previous versions (docs/16-snapshots.md, docs/01, 02, 06, 07,
// 11, 12 and 13): designed 2026-10-11, docs and lookdev first. Each stub is deleted in
// the commit that lands the real tests named in its doc's Tests table (PR 1 meta, PR 2
// gateways and the tick, PR 3 the web UI, PR 4 soak phase 19).

// Invariant: I-179
// Becomes: TestFSM_Snapshot_Table, TestStore_ReadsAt_Table, TestFSM_ReadDirAt_Determinism (PR 1).
func TestStub_I_179(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/16-snapshots.md") }

// Invariant: I-180
// Becomes: TestFSM_SpanRule_Table, TestFSM_Commit_PinnedKeepsVersion, TestFSM_Fold_KeepsDeletedSpan (PR 1).
func TestStub_I_180(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/16-snapshots.md") }

// Invariant: I-181
// Becomes: TestFSM_SnapshotReap_Table, TestFSM_SnapshotReap_LimitBoundary, TestGC_ReapThenCollect (PR 1, PR 2).
func TestStub_I_181(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/16-snapshots.md") }

// Invariant: I-182
// Becomes: TestFSM_Purge_SkipsPinnedRoot, TestFSM_Purge_DropsVersionRefs, TestGC_PurgeTrash_PinnedRootWaits (PR 1, PR 2).
func TestStub_I_182(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/06-gc-and-repair.md") }

// Invariant: I-183
// Becomes: TestSchedule_TakesAndThins_Table, TestSchedule_ManualNeverThinned, TestConfig_Snapshots (PR 2).
func TestStub_I_183(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/06-gc-and-repair.md") }

// Invariant: I-184
// Becomes: TestCodec_ReadsVersion4Snapshot, TestEncodeCommand_V5Gating, TestFSM_HistoryStats_MatchComputed (PR 1).
func TestStub_I_184(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/01-data-model.md") }

// Invariant: I-185
// Becomes: TestNFS_SnapshotsDir_HiddenResolvesLists, TestNFS_SnapshotHandle_StaleTable, TestSMB_SnapshotsDir_ReadOnly (PR 2).
func TestStub_I_185(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/07-nfs-gateway.md") }

// Invariant: I-186
// Becomes: TestVFS_Revert_Table, TestVFS_RestoreTree_SkipsExisting, TestFSM_Commit_ResultKept (PR 1, PR 2).
func TestStub_I_186(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/16-snapshots.md") }

// Invariant: I-187
// Becomes: TestWebUI_Snapshots_ListTakeDelete, TestWebUI_LsAt_ReadOnly, TestWebUI_Versions_DistinctContents (PR 3).
func TestStub_I_187(t *testing.T) { t.Skip("stub until snapshots PR 3; see docs/13-web-ui.md") }

// Invariant: I-188
// Becomes: TestCLI_Export, TestCLI_Snapshot_LsCreateRmRestore, TestReport_SnapshotFields, TestCheck_SnapshotScheduleStale (PR 2).
func TestStub_I_188(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/12-operations.md") }

// Lifecycle: L-212
// Committed (sole reference) --[Commit overwrite or truncate while pinned]--> Committed (held by a version)
func TestStubLifecycle_L_212(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-212") }

// Lifecycle: L-213
// Committed (held only by a version) --[E-133 reap]--> Tombstoned
func TestStubLifecycle_L_213(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-213") }

// Lifecycle: L-214
// Committed (shared: live and a version) --[E-133 reap]--> Committed (Refs−1 ≥ 1)
func TestStubLifecycle_L_214(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-214") }

// Lifecycle: L-215
// Committed (in the trash, root pinned) --[E-128 Purge]--> Committed (unchanged)
func TestStubLifecycle_L_215(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-215") }

// Lifecycle: L-216
// forbidden: Committed (referenced only from a version or a pinned root) --[GCDelete, or a reap while pinned]--> Collected
func TestStubLifecycle_L_216(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-216") }

// Lifecycle: L-217
// Committed (a version's chunks) --[revert: Commit by hash]--> Committed (Refs+1 per position, no upload)
func TestStubLifecycle_L_217(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/11 L-217") }

// Lifecycle: L-218
// Committed (held by a version inside a trashed subtree no snapshot reaches) --[E-128 Purge]--> Tombstoned
func TestStubLifecycle_L_218(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-218") }
