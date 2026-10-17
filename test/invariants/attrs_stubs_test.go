package invariants

import "testing"

// Stubs for a file that lands whole (docs/04 §6). Each is deleted in the commit that lands
// the tests named in that doc's Tests table.

// Invariant: I-401
func TestStub_I_401(t *testing.T) {
	t.Skip("stub: SETATTR on a dirty file waits for its flush; see docs/04 §6")
}

// Invariant: I-402
func TestStub_I_402(t *testing.T) { t.Skip("stub: a no-op SETATTR proposes nothing; see docs/04 §6") }

// Invariant: I-403
func TestStub_I_403(t *testing.T) { t.Skip("stub: proposal counters in status; see docs/04 §6") }
