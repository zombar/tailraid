package invariants

import "testing"

// Stubs for the streaming large-file work (docs/04 §9, docs/13 §4, docs/11 L-188..L-191).
// Each is deleted in the commit that lands the real test named in its comment.

// Invariant: I-81
// Becomes: TestWebUI_Put_AtomicOverwrite_DisconnectKeepsOld, TestWebUI_Put_Parts_Table, TestWebUI_SweepUploads_AgeTable.
func TestStub_I_81(t *testing.T) { t.Skip("stub until the atomic upload lands; docs/13 §4") }

// Invariant: I-82
// Becomes: TestWebUI_Put_NoContentLength_411, TestWebUI_Put_FreeSpace_507, TestWebUI_Put_Concurrency_503, TestWebUI_Put_BodyStall_DropsTemp.
func TestStub_I_82(t *testing.T) { t.Skip("stub until the upload limits land; docs/13 §4") }

// Invariant: I-84
// Becomes: TestReport_ProcessAndFlushFields.
func TestStub_I_84(t *testing.T) { t.Skip("stub until the process fields land; docs/12 §8") }

// Lifecycle: L-188
// Becomes: TestOverlay_WALCap_WaitsForFlush.
func TestStub_L_188(t *testing.T) { t.Skip("stub until cap backpressure lands") }

// Lifecycle: L-189
// Becomes: TestOverlay_WALCap_FailsFastWhenFlushFailed.
func TestStub_L_189(t *testing.T) { t.Skip("stub until cap backpressure lands") }
