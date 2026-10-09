package invariants

import "testing"

// Stubs for the SMB gateway (docs/15-smb-gateway.md). Each carries the name of the real
// test and is deleted in the commit that lands it in internal/smbgw, internal/vfs,
// internal/config or cmd/tailraid.

const smbStub = "stub until the SMB gateway lands; see docs/15-smb-gateway.md"

// Invariant: I-106
func TestSMB_WriteSequence_MultiRequest(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-106
func TestSMB_WriteSize_Boundaries(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-106
func TestVFS_WriteAt_ExactOffset_PosUnchanged(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-107
func TestSMB_SetInfo_EndOfFile(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-107, I-108
func TestSMB_RenameKeepsFileIdAndContent(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-108
func TestSMB_QueryDirectory_LargeDirectory(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-109
func TestSMB_ReadOnly_WireStatuses(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-109
func TestSMB_WriteWhenFull_IsDiskFull(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-109
func TestSMB_WriteAtCap_WaitsThenSucceeds(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-109
func TestSMB_Statfs_FromUsage(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-110
func TestSMB_Auth_WrongPasswordRefused(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-110, I-50
func TestSMB_ListenRefusesNonLoopback(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-110
func TestConfig_Gateways(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-110
func TestCLI_Init_GeneratesSMBPassword(t *testing.T) { t.Skip(smbStub) }
