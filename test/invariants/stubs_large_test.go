package invariants

import "testing"

// Stubs for the streaming large-file work (docs/04 §9, docs/13 §4, docs/11 L-188..L-191).
// Each is deleted in the commit that lands the real test named in its comment.

// Invariant: I-84
// Becomes: TestReport_ProcessAndFlushFields.
func TestStub_I_84(t *testing.T) { t.Skip("stub until the process fields land; docs/12 §8") }
