package invariants

import "testing"

// Stubs for watching and the media search (docs/13 §10a–§10d, docs/02 op 34, docs/18 §6a),
// designed on 2026-10-17. Each is deleted by the PR that lands the tests its doc's Tests
// table names: playback-meta (I-405, I-406, landed), media-search-index (I-411..I-413, landed) and
// media-viewer-web (I-407..I-410, I-414).

// Invariant: I-407
func TestStub_I_407(t *testing.T) { t.Skip("stub: the playback viewer; see docs/13 §10c") }

// Invariant: I-408
func TestStub_I_408(t *testing.T) { t.Skip("stub: playback coalescing; see docs/13 §10c") }

// Invariant: I-409
func TestStub_I_409(t *testing.T) { t.Skip("stub: Continue watching; see docs/13 §10c") }

// Invariant: I-410
func TestStub_I_410(t *testing.T) { t.Skip("stub: progress on media items; see docs/13 §10c") }

// Invariant: I-414
func TestStub_I_414(t *testing.T) { t.Skip("stub: the viewer routes; see docs/13 §10a, §10b") }
