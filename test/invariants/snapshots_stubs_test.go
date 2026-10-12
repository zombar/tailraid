package invariants

import "testing"

// Stubs for snapshots on mirror disks (docs/16-snapshots.md, docs/06, 07, 11, 12 and 13)
// whose tests land after PR 1 (the catalogue, the hold, codec 7 and the mirror store).
// Each stub is deleted in the commit that lands the real tests named in its doc's Tests
// table (PR 2 sync, the tick, the gateways and the CLI, PR 3 the web UI, PR 4 soak
// phase 19).

// Invariant: I-187
// Becomes: TestWebUI_Snapshots_ListTake_NoDeleteRoute, TestWebUI_LsAt_ReadOnly, TestWebUI_OfflineMirror_409 (PR 3).
func TestStub_I_187(t *testing.T) { t.Skip("stub until snapshots PR 3; see docs/13-web-ui.md") }
