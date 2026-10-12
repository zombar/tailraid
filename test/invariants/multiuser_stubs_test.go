package invariants

import "testing"

// Stubs for many users at once (docs/02, 04, 13 and 15; soak phase 23 in docs/09 §6).
// Each stub is deleted in the commit that lands the real tests named in its doc's Tests
// table (PR excl-create, whole-rewrite, smb-handles, web-upload-race, soak-multiuser).

// Invariant: I-226
// Becomes: TestWebUI_Put_TwoGateways_OneName_OneWhole, TestWebUI_Put_TwoGateways_IfNoneMatch_One412 (PR web-upload-race).
func TestStub_I_226(t *testing.T) {
	t.Skip("stub until PR web-upload-race; see docs/13-web-ui.md")
}
