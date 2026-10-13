package invariants

import "testing"

// Stubs for the secrets store (docs/19): PR 2 lands I-303..I-307, PR 3 I-308 (PR 1 landed I-309).

// Invariant: I-303
func TestStub_I_303(t *testing.T) {
	t.Skip("stub: the cluster key stays in the keychain (docs/19)")
}

// Invariant: I-304
func TestStub_I_304(t *testing.T) {
	t.Skip("stub: the member key fetch (docs/19)")
}

// Invariant: I-305
func TestStub_I_305(t *testing.T) {
	t.Skip("stub: one key per cluster (docs/19)")
}

// Invariant: I-306
func TestStub_I_306(t *testing.T) {
	t.Skip("stub: sealed values (docs/19)")
}

// Invariant: I-307
func TestStub_I_307(t *testing.T) {
	t.Skip("stub: the secret ops and their bounds (docs/19)")
}

// Invariant: I-308
func TestStub_I_308(t *testing.T) {
	t.Skip("stub: S3 and TMDB keys move into the store (docs/19)")
}
