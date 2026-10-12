package invariants

import "testing"

// Stubs for snapshots on mirror disks (docs/16-snapshots.md, docs/01, 02, 06, 07, 11, 12
// and 13): designed 2026-10-12, docs and lookdev first. Each stub is deleted in the
// commit that lands the real tests named in its doc's Tests table (PR 1 meta and the
// mirror store, PR 2 sync, the tick, the gateways and the CLI, PR 3 the web UI, PR 4 soak
// phase 19).

// Invariant: I-179
// Becomes: TestFSM_SnapshotBegin_Table, TestMirror_SyncCapturesIndex, TestCluster_SnapshotReadsAsOfIndexOnEveryGateway (PR 1, PR 2).
func TestStub_I_179(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/16-snapshots.md") }

// Invariant: I-180
// Becomes: TestMirror_TreeEncoding_Canonical, TestMirror_SecondSyncWritesOnlyChanges, TestMirror_CrashAtEveryWrite_ManifestImpliesComplete (PR 1).
func TestStub_I_180(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/16-snapshots.md") }

// Invariant: I-181
// Becomes: TestFSM_GCDelete_HeldBySyncingRow, TestFSM_SnapshotEnd_Table, TestCluster_OverwriteDuringSync_SnapshotExact (PR 1, PR 2).
func TestStub_I_181(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/16-snapshots.md") }

// Invariant: I-182
// Becomes: TestSchedule_TakesAndThins_Table, TestSchedule_CatchUpOnAttach, TestSchedule_OfflineMirrorNothingHeld (PR 2).
func TestStub_I_182(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/06-gc-and-repair.md") }

// Invariant: I-183
// Becomes: TestAdminOps_NoSnapshotDelete, TestMirror_DeleteReconciles_MarkAndSweep, TestMirror_RowlessManifestImported (PR 1, PR 2).
func TestStub_I_183(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/16-snapshots.md") }

// Invariant: I-184
// Becomes: TestCodec_ReadsVersion6Snapshot, TestEncodeCommand_V7Gating, TestCandidates_MirrorNeverATarget (PR 1).
func TestStub_I_184(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/01-data-model.md") }

// Invariant: I-185
// Becomes: TestNFS_SnapshotsDir_HiddenResolvesLists, TestNFS_SnapshotRead_PoolThenMirror, TestNFS_SnapshotHandle_StaleTable (PR 2).
func TestStub_I_185(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/07-nfs-gateway.md") }

// Invariant: I-186
// Becomes: TestVFS_Revert_FromPoolAndMirror, TestVFS_RestoreTree_SkipsExisting, TestCluster_RestoreAfterGC_UploadsFromMirror (PR 2).
func TestStub_I_186(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/16-snapshots.md") }

// Invariant: I-187
// Becomes: TestWebUI_Snapshots_ListTake_NoDeleteRoute, TestWebUI_LsAt_ReadOnly, TestWebUI_OfflineMirror_409 (PR 3).
func TestStub_I_187(t *testing.T) { t.Skip("stub until snapshots PR 3; see docs/13-web-ui.md") }

// Invariant: I-188
// Becomes: TestCLI_MirrorAdd_MarkRehomeAdopt, TestCLI_Export, TestCheck_MirrorWarnings, TestConfig_Mirrors (PR 2).
func TestStub_I_188(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/12-operations.md") }

// Lifecycle: L-212
// Committed (referenced by a syncing snapshot) --[overwrite, truncate or purge after E-131]--> Tombstoned (held)
func TestStubLifecycle_L_212(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-212") }

// Lifecycle: L-213
// Tombstoned (held) --[E-132 SnapshotEnd]--> Tombstoned → Collected after grace
func TestStubLifecycle_L_213(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-213") }

// Lifecycle: L-214
// Committed --[E-131 sync reads it]--> Committed (and one object on the mirror)
func TestStubLifecycle_L_214(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/11 L-214") }

// Lifecycle: L-215
// Unreadable --[E-131 sync reads it]--> Unreadable (listed missing; the row ends Incomplete)
func TestStubLifecycle_L_215(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/11 L-215") }

// Lifecycle: L-216
// Collected, an object on a mirror --[E-133 restore]--> Uploading → Committed
func TestStubLifecycle_L_216(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/11 L-216") }

// Lifecycle: L-217
// Committed or Tombstoned (still in the pool) --[E-133 restore]--> Committed by hash, no upload
func TestStubLifecycle_L_217(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/11 L-217") }

// Lifecycle: L-218
// forbidden: Tombstoned at or after a syncing row's At --[GCDelete]--> Collected
func TestStubLifecycle_L_218(t *testing.T) { t.Skip("stub until snapshots PR 1; see docs/11 L-218") }
