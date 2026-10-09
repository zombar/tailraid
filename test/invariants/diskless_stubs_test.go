package invariants

import "testing"

// Stubs for diskless hosts (docs/14 §9). Each carries the name of the real test and is
// deleted in the commit that lands it.

const disklessStub = "stub until devices stop being targets; see docs/14-disks.md §9"

// Invariant: I-118
func TestCandidates_DevicesNeverTargets(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-118
func TestUsage_DisksOnly(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-118
func TestAutoPolicy_DisklessHostNotCounted(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-118
func TestCluster_DisklessVoter_WritesLandOnOtherHosts(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-118
func TestCluster_RemoveLastDisk_HostBecomesDiskless(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-118
func TestMemberOp_SetCapacity_RefusesDevice(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-119
func TestLegacyDeviceTargets_Table(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-119
func TestServe_RefusesLegacyState(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-119
func TestCLI_Serve_RefusesLegacyState(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-120
func TestHasTargets_Table(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-120
func TestCluster_Diskless_WriteIsENOSPC(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-120
func TestFlusher_NoTargets_ErrorWrapsErrFull(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-121
func TestCLI_Join_RegistersConfiguredDisks(t *testing.T) { t.Skip(disklessStub) }

// Invariant: I-121
func TestCLI_Init_NoDisks_PrintsHint(t *testing.T) { t.Skip(disklessStub) }
