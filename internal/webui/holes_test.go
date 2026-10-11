package webui_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/testharness"
)

// A sparse file's holes are zero chunks: their reports carry `slots` as an empty array,
// never null, so the panel's slot strip has something to iterate (I-161).
//
// Invariant: I-161
func TestWebUI_Chunks_HolesHaveSlots(t *testing.T) {
	u := newUI(t)
	cs := int64(testharness.ChunkSize)
	u.write("/sparse", 2*cs, testharness.Pattern(int(cs), 3)) // chunks 0 and 1 are holes
	u.flush()
	code, _, body := u.get("/api/chunks?path=/sparse")
	if code != 200 {
		t.Fatalf("chunks: %d %s", code, body)
	}
	if strings.Contains(string(body), `"slots":null`) {
		t.Fatalf("a report carries null slots: %s", body)
	}
	var page struct {
		Total  int                 `json:"total"`
		Chunks []admin.ChunkReport `json:"chunks"`
	}
	must(t, json.Unmarshal(body, &page))
	if page.Total != 3 || len(page.Chunks) != 3 {
		t.Fatalf("page %+v", page)
	}
	for i, c := range page.Chunks[:2] {
		if c.State != "zero" || c.Slots == nil || len(c.Slots) != 0 {
			t.Fatalf("hole %d: %+v", i, c)
		}
	}
	if c := page.Chunks[2]; !c.Found || len(c.Slots) != 3 {
		t.Fatalf("data chunk: %+v", c)
	}
}
