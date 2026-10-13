package invariants

import "testing"

// Stubs for `tailraid media scrape` (docs/18 §11): the media-scrape PR lands the tests.

// Invariant: I-298
func TestStub_I_298(t *testing.T) {
	t.Skip("stub: no node contacts a metadata provider (docs/18 §11)")
}

// Invariant: I-299
func TestStub_I_299(t *testing.T) {
	t.Skip("stub: a scrape writes only missing slots, as exclusive creates (docs/18 §11)")
}

// Invariant: I-300
func TestStub_I_300(t *testing.T) {
	t.Skip("stub: what a scrape writes, the catalogue reads back (docs/18 §11)")
}

// Invariant: I-301
func TestStub_I_301(t *testing.T) {
	t.Skip("stub: clear matches only, else the user's choice (docs/18 §11)")
}

// Invariant: I-302
func TestStub_I_302(t *testing.T) {
	t.Skip("stub: the TMDB key and the client's network hygiene (docs/18 §11)")
}
