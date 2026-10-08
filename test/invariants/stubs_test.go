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

// Invariant: I-76
// Becomes: TestCLI_Init_NoFlags (docs/12).
func TestStub_I_76(t *testing.T) {
	t.Skip("stub until init-without-flags lands; see docs/12")
}
