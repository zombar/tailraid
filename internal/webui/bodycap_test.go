package webui_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/webui"
)

// POST /api/mkdir reads at most 4 KiB: the bound and one under create the directory, one
// over is a 400 that names the size.
//
// Invariant: I-116
func TestWebUI_MkdirBodyCap(t *testing.T) {
	u := newUI(t)
	const limit = 4096
	for i, n := range []int{limit - 1, limit, limit + 1} {
		head, tail := fmt.Sprintf(`{"path":"/cap%d","pad":"`, i), `"}`
		body := head + strings.Repeat("a", n-len(head)-len(tail)) + tail
		resp := u.do(http.MethodPost, "/api/mkdir", strings.NewReader(body), webui.HeaderUI, "1")
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := http.StatusCreated
		if n > limit {
			want = http.StatusBadRequest
		}
		if resp.StatusCode != want || (n > limit && !strings.Contains(string(b), "too large")) {
			t.Fatalf("%d-byte body: %d %s, want %d", n, resp.StatusCode, b, want)
		}
	}
}
