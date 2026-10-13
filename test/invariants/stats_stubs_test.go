package invariants

import "testing"

// Stubs for the stats page (docs/17, docs/12 §8, docs/13, docs/06, docs/08): each is deleted
// by the PR that lands the tests its doc's Tests table names after it. PR 1 (the chart
// package, I-197), PR 2 (the network vitals, I-190) and PR 3 (the store, I-191 to I-194,
// I-199, and the hiding half of I-195) and PR 4 (the web route, the page and the theme
// switch, I-196, I-198, I-200) have landed; the ephemeral half of I-195 waits for the
// trash of docs/02.

// Invariant: I-195
func TestStub_I_195(t *testing.T) {
	t.Skip("stub: /.tailraid is ephemeral once the trash lands (docs/02, docs/06)")
}
