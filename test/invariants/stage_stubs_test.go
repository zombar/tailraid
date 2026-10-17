package invariants

import "testing"

// Stubs for the stage and the segmented WAL of docs/04 §2.1 and §3. Each is deleted in the
// commit that lands the tests named in that doc's Tests table.

// Invariant: I-396
func TestStub_I_396(t *testing.T) { t.Skip("stub: the stage; see docs/04 §3") }

// Invariant: I-397
func TestStub_I_397(t *testing.T) { t.Skip("stub: the segmented WAL; see docs/04 §2.1") }

// Invariant: I-398
func TestStub_I_398(t *testing.T) { t.Skip("stub: staged bytes in status; see docs/04 §3") }

// Invariant: I-399
func TestStub_I_399(t *testing.T) { t.Skip("stub: extents append in place; see docs/04 §9") }
