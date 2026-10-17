package invariants

import "testing"

// Stubs for watching and the media search (docs/13 §10a–§10d, docs/02 op 34, docs/18 §6a),
// designed on 2026-10-17. Each is deleted by the PR that lands the tests its doc's Tests
// table names: playback-meta (I-405, I-406), media-search-index (I-411..I-413) and
// media-viewer-web (I-407..I-410, I-414).

// Invariant: I-405
func TestStub_I_405(t *testing.T) { t.Skip("stub: playback rows; see docs/02 §Apply rules") }

// Invariant: I-406
func TestStub_I_406(t *testing.T) { t.Skip("stub: codec 17 and frame 16; see docs/02 §Apply rules") }

// Invariant: I-407
func TestStub_I_407(t *testing.T) { t.Skip("stub: the playback viewer; see docs/13 §10c") }

// Invariant: I-408
func TestStub_I_408(t *testing.T) { t.Skip("stub: playback coalescing; see docs/13 §10c") }

// Invariant: I-409
func TestStub_I_409(t *testing.T) { t.Skip("stub: Continue watching; see docs/13 §10c") }

// Invariant: I-410
func TestStub_I_410(t *testing.T) { t.Skip("stub: progress on media items; see docs/13 §10c") }

// Invariant: I-411
func TestStub_I_411(t *testing.T) { t.Skip("stub: the query grammar; see docs/18 §6a") }

// Invariant: I-412
func TestStub_I_412(t *testing.T) { t.Skip("stub: field terms and scopes; see docs/18 §6a") }

// Invariant: I-413
func TestStub_I_413(t *testing.T) { t.Skip("stub: suggestions; see docs/18 §6a") }

// Invariant: I-414
func TestStub_I_414(t *testing.T) { t.Skip("stub: the viewer routes; see docs/13 §10a, §10b") }
