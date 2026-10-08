// Package invariants holds skipped placeholder tests so that every invariant defined in
// docs/*.md is cited from the first commit. Each stub is deleted in the same commit that
// lands the real test(s) named in its comment. scripts/check-invariants.sh enforces the
// mapping; see docs/09-testing-strategy.md.
package invariants

import "testing"

// Invariant: I-2
// Becomes: TestStore_PutRejectsHashMismatch, TestVFS_DedupeIdenticalChunks (M1).
func TestStub_I_2(t *testing.T) { t.Skip("stub until M1; see docs/01-data-model.md") }

// Invariant: I-15
// Becomes: TestCluster_NodeDiesMidFlush_RepairRestoresRedundancy, TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK (M3).
func TestStub_I_15(t *testing.T) { t.Skip("stub until M3; see docs/06-gc-and-repair.md") }

// Invariant: I-19
// Becomes: TestS3NFS_OracleBothFrontEnds (M5a).
func TestStub_I_19(t *testing.T) { t.Skip("stub until M5a; see docs/10-s3-nodes.md") }

// Invariant: I-20
// Becomes: TestS3_PutReturnsAfterCloseDurable (M5a).
func TestStub_I_20(t *testing.T) { t.Skip("stub until M5a; see docs/10-s3-nodes.md") }

// Invariant: I-21
// Becomes: TestS3_ListObjectsV2_CompleteOrderedUnderConcurrentWrites (M5a).
func TestStub_I_21(t *testing.T) { t.Skip("stub until M5a; see docs/10-s3-nodes.md") }

// Invariant: I-45
// Becomes: TestNFS_WriteSequence_MultiRPC, TestVFS_PerRPCOpenCloseSequence, TestVFS_WriteAtNonZeroOffset_PreservesPrefix (M1).
func TestStub_I_45(t *testing.T) { t.Skip("stub until M1; see docs/07-nfs-gateway.md") }

// Invariant: I-46
// Becomes: TestNFS_SetattrModeOwnerTimes, TestVFS_ChangeOps (M1).
func TestStub_I_46(t *testing.T) { t.Skip("stub until M1; see docs/07-nfs-gateway.md") }

// Invariant: I-47
// Becomes: TestNFS_ReadDirPlusLargeDirectory (M1).
func TestStub_I_47(t *testing.T) { t.Skip("stub until M1; see docs/07-nfs-gateway.md") }

// Invariant: I-49
// Becomes: TestIDStream_RejectsForeignCluster, TestCluster_TwoInitsDoNotMerge (M2).
func TestStub_I_49(t *testing.T) { t.Skip("stub until M2; see docs/08-security.md") }

// Invariant: I-50
// Becomes: TestRPC_ListenRefusesNonLoopbackWithoutAuth, TestNFSGW_BindsLoopbackOnly, TestServe_ListenerAddresses (M1/M2).
func TestStub_I_50(t *testing.T) { t.Skip("stub until M2; see docs/08-security.md") }
