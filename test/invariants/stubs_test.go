// Package invariants holds skipped placeholder tests so that every invariant defined in
// docs/*.md is cited from the first commit. Each stub is deleted in the same commit that
// lands the real test(s) named in its comment. scripts/check-invariants.sh enforces the
// mapping; see docs/09-testing-strategy.md.
package invariants

import "testing"

// Invariant: I-1
// Becomes: TestVFS_OracleRandomOps, TestFSM_SetChunks_SizeAndChunkListConsistent (M1).
func TestStub_I_1(t *testing.T) { t.Skip("stub until M1; see docs/01-data-model.md") }

// Invariant: I-2
// Becomes: TestStore_PutRejectsHashMismatch, TestVFS_DedupeIdenticalChunks (M1).
func TestStub_I_2(t *testing.T) { t.Skip("stub until M1; see docs/01-data-model.md") }

// Invariant: I-4
// Becomes: TestFSM_DeterministicAcrossReplicas, TestCluster_LeaderFailoverMidApply_NoLostOrDuplicateCommits (M1/M2).
func TestStub_I_4(t *testing.T) { t.Skip("stub until M1; see docs/02-consistency-and-raft.md") }

// Invariant: I-5
// Becomes: TestFSM_ApplySnapshotRestoreEquivalence, TestCluster_RejoinAfterLongAbsence_SnapshotInstall (M1/M2).
func TestStub_I_5(t *testing.T) { t.Skip("stub until M1; see docs/02-consistency-and-raft.md") }

// Invariant: I-6
// Becomes: TestCluster_SplitBrain_OnlyMajorityCommits (M2).
func TestStub_I_6(t *testing.T) { t.Skip("stub until M2; see docs/02-consistency-and-raft.md") }

// Invariant: I-7
// Becomes: TestVFS_CloseFlushIsDurable, TestNFS_WriteSequence_MultiRPC, TestCrash_AtEveryPoint (M1/M2).
func TestStub_I_7(t *testing.T) { t.Skip("stub until M1; see docs/04-write-path-and-durability.md") }

// Invariant: I-11
// Becomes: TestStore_Get_CorruptedBitFlip, TestStore_CrashBeforeRename_LeavesNoVisibleShard, TestStore_TornWrite_DetectedOnRead (M1).
func TestStub_I_11(t *testing.T) { t.Skip("stub until M1; see docs/04-write-path-and-durability.md") }

// Invariant: I-12
// Becomes: TestCluster_KillMNodes_AllDataReadable, TestDurability_AckImpliesReadableFromKSurvivors (M2/M3).
func TestStub_I_12(t *testing.T) { t.Skip("stub until M2; see docs/05-failure-model.md") }

// Invariant: I-13
// Becomes: TestCluster_GCDoesNotDeleteInflightUpload, TestModelCheck_Long (M3).
func TestStub_I_13(t *testing.T) { t.Skip("stub until M3; see docs/06-gc-and-repair.md") }

// Invariant: I-14
// Becomes: TestCluster_GCDoesNotDeleteInflightUpload, TestSweep_RespectsGrace (M3).
func TestStub_I_14(t *testing.T) { t.Skip("stub until M3; see docs/06-gc-and-repair.md") }

// Invariant: I-15
// Becomes: TestCluster_NodeDiesMidFlush_RepairRestoresRedundancy, TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK (M3).
func TestStub_I_15(t *testing.T) { t.Skip("stub until M3; see docs/06-gc-and-repair.md") }

// Invariant: I-16
// Becomes: TestNFS_RenameKeepsHandleValid, TestNFS_HandleSurvivesGatewayRestart, TestFSM_HandleStableAcrossRestart (M1).
func TestStub_I_16(t *testing.T) { t.Skip("stub until M1; see docs/07-nfs-gateway.md") }

// Invariant: I-17
// Becomes: TestNFS_RemovedHandleIsStale, TestNFS_BogusHandleIsStaleNotPanic (M1).
func TestStub_I_17(t *testing.T) { t.Skip("stub until M1; see docs/07-nfs-gateway.md") }

// Invariant: I-18
// Becomes: TestWhoIs_DeniesUnknownPeer, TestChunkRPC_ReadsAuthBeforeBody, TestRPC_ClusterIDMismatch409 (M2).
func TestStub_I_18(t *testing.T) { t.Skip("stub until M2; see docs/08-security.md") }

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

// Invariant: I-48
// Becomes: TestJoin_WrongToken403_NoRaftChange, TestJoin_StableIDMismatch403, TestJoin_Succeeds_AddsNonvoter (M2).
func TestStub_I_48(t *testing.T) { t.Skip("stub until M2; see docs/08-security.md") }

// Invariant: I-49
// Becomes: TestIDStream_RejectsForeignCluster, TestCluster_TwoInitsDoNotMerge (M2).
func TestStub_I_49(t *testing.T) { t.Skip("stub until M2; see docs/08-security.md") }

// Invariant: I-50
// Becomes: TestRPC_ListenRefusesNonLoopbackWithoutAuth, TestNFSGW_BindsLoopbackOnly, TestServe_ListenerAddresses (M1/M2).
func TestStub_I_50(t *testing.T) { t.Skip("stub until M2; see docs/08-security.md") }
