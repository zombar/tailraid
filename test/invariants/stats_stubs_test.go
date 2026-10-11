package invariants

import "testing"

// Stubs for the stats page (docs/17, docs/12 §8, docs/13, docs/06, docs/08): each is deleted
// by the PR that lands the tests its doc's Tests table names after it. PR 1 is the chart
// package (I-197), PR 2 the network vitals (I-190), PR 3 the store (I-191 to I-195, I-199),
// PR 4 the web route, the page and the theme switch (I-196, I-198, I-200).

// Invariant: I-190
func TestStub_I_190(t *testing.T) { t.Skip("stub: stats PR 2 (network vitals); see docs/12 §8") }

// Invariant: I-191
func TestStub_I_191(t *testing.T) { t.Skip("stub: stats PR 3 (sampler); see docs/17") }

// Invariant: I-192
func TestStub_I_192(t *testing.T) { t.Skip("stub: stats PR 3 (segments); see docs/17") }

// Invariant: I-193
func TestStub_I_193(t *testing.T) { t.Skip("stub: stats PR 3 (rollups); see docs/17") }

// Invariant: I-194
func TestStub_I_194(t *testing.T) { t.Skip("stub: stats PR 3 (retention); see docs/17") }

// Invariant: I-195
func TestStub_I_195(t *testing.T) { t.Skip("stub: stats PR 3 (ephemeral /.tailraid); see docs/06") }

// Invariant: I-196
func TestStub_I_196(t *testing.T) { t.Skip("stub: stats PR 4 (/api/stats); see docs/17") }

// Invariant: I-197
func TestStub_I_197(t *testing.T) { t.Skip("stub: stats PR 1 (chart package); see docs/13") }

// Invariant: I-198
func TestStub_I_198(t *testing.T) { t.Skip("stub: stats PR 4 (the page); see docs/13") }

// Invariant: I-199
func TestStub_I_199(t *testing.T) { t.Skip("stub: stats PR 3 (/v1/stats); see docs/08") }

// Invariant: I-200
func TestStub_I_200(t *testing.T) { t.Skip("stub: stats PR 4 (theme switch); see docs/13") }
