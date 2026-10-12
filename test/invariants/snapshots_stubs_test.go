package invariants

import "testing"

// Stubs for snapshots on mirror disks (docs/16-snapshots.md, docs/06, 07, 11, 12 and 13)
// whose tests land after PR 1 (the catalogue, the hold, codec 7 and the mirror store).
// Each stub is deleted in the commit that lands the real tests named in its doc's Tests
// table (PR 2 sync, the tick, the gateways and the CLI, PR 3 the web UI, PR 4 soak
// phase 19).

// Invariant: I-185
// Becomes: TestNFS_SnapshotsDir_HiddenResolvesLists, TestNFS_SnapshotRead_PoolThenMirror, TestNFS_SnapshotHandle_StaleTable (PR 2).
func TestStub_I_185(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/07-nfs-gateway.md") }

// Invariant: I-186
// Becomes: TestVFS_Revert_FromPoolAndMirror, TestVFS_RestoreTree_SkipsExisting, TestCluster_RestoreAfterGC_UploadsFromMirror (PR 2).
func TestStub_I_186(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/16-snapshots.md") }

// Invariant: I-187
// Becomes: TestWebUI_Snapshots_ListTake_NoDeleteRoute, TestWebUI_LsAt_ReadOnly, TestWebUI_OfflineMirror_409 (PR 3).
func TestStub_I_187(t *testing.T) { t.Skip("stub until snapshots PR 3; see docs/13-web-ui.md") }

// Lifecycle: L-216
// Collected, an object on a mirror --[E-133 restore]--> Uploading → Committed
func TestStubLifecycle_L_216(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/11 L-216") }

// Lifecycle: L-217
// Committed or Tombstoned (still in the pool) --[E-133 restore]--> Committed by hash, no upload
func TestStubLifecycle_L_217(t *testing.T) { t.Skip("stub until snapshots PR 2; see docs/11 L-217") }
