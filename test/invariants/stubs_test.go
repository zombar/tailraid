// Package invariants holds skipped placeholder tests so that every invariant defined in
// docs/*.md is cited from the first commit. Each stub is deleted in the same commit that
// lands the real test(s) named in its comment. scripts/check-invariants.sh enforces the
// mapping; see docs/09-testing-strategy.md.
package invariants

import "testing"

// Invariant: I-19
// Becomes: TestS3NFS_OracleBothFrontEnds (M5a).
func TestStub_I_19(t *testing.T) { t.Skip("stub until M5a; see docs/10-s3-nodes.md") }

// Invariant: I-20
// Becomes: TestS3_PutReturnsAfterCloseDurable (M5a).
func TestStub_I_20(t *testing.T) { t.Skip("stub until M5a; see docs/10-s3-nodes.md") }

// Invariant: I-21
// Becomes: TestS3_ListObjectsV2_CompleteOrderedUnderConcurrentWrites (M5a).
func TestStub_I_21(t *testing.T) { t.Skip("stub until M5a; see docs/10-s3-nodes.md") }

// Invariant: I-68
// Becomes: TestWebUI_Authorize_MemberOrSameUser (docs/13).
func TestStub_I_68(t *testing.T) {
	t.Skip("stub until the web UI handler lands; see docs/13-web-ui.md")
}

// Invariant: I-69
// Becomes: TestWebUI_Get_RangeBoundaries (docs/13).
func TestStub_I_69(t *testing.T) {
	t.Skip("stub until the web UI handler lands; see docs/13-web-ui.md")
}

// Invariant: I-70
// Becomes: TestWebUI_Put_NewAndOverwrite (docs/13).
func TestStub_I_70(t *testing.T) {
	t.Skip("stub until the web UI handler lands; see docs/13-web-ui.md")
}

// Invariant: I-71
// Becomes: TestWebUI_CSRFHeaderRequired (docs/13).
func TestStub_I_71(t *testing.T) {
	t.Skip("stub until the web UI handler lands; see docs/13-web-ui.md")
}

// Invariant: I-72
// Becomes: TestWebUI_Paths (docs/13).
func TestStub_I_72(t *testing.T) {
	t.Skip("stub until the web UI handler lands; see docs/13-web-ui.md")
}

// Invariant: I-73
// Becomes: TestWebUI_StaticAssets (docs/13).
func TestStub_I_73(t *testing.T) {
	t.Skip("stub until the web UI handler lands; see docs/13-web-ui.md")
}
