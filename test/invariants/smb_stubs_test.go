package invariants

import "testing"

// Stubs for the SMB gateway (docs/15-smb-gateway.md). Each carries the name of the real
// test and is deleted in the commit that lands it in internal/config or cmd/tailraid.

const smbStub = "stub until the SMB gateway lands; see docs/15-smb-gateway.md"

// Invariant: I-110
func TestConfig_Gateways(t *testing.T) { t.Skip(smbStub) }

// Invariant: I-110
func TestCLI_Init_GeneratesSMBPassword(t *testing.T) { t.Skip(smbStub) }
