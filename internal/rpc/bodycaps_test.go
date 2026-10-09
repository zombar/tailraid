package rpc

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Every request body is bounded before it is decoded: one byte under the bound and the
// bound itself are decoded, one byte over is a 400 that names the decode, whatever the
// handler would have said about the content.
//
// Invariant: I-116
func TestRPC_BodyCaps_Table(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for _, r := range []struct {
		path    string
		limit   int
		json    bool
		refused string
	}{
		{"/v1/apply", 4 << 20, false, "read body"},
		{"/v1/join", 64 << 10, true, "bad join request"},
		{"/v1/members", 64 << 10, true, "bad member op"},
		{"/v1/bad-slot", 4 << 10, true, "bad report"},
		{"/v1/admin", 64 << 10, true, "bad admin op"},
	} {
		for _, n := range []int{r.limit - 1, r.limit, r.limit + 1} {
			var body []byte
			if r.json { // one JSON value spanning the whole body, so the decoder must read it all
				head, tail := `{"op":"x","pad":"`, `"}`
				body = []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
			} else {
				body = bytes.Repeat([]byte{0xff}, n)
			}
			resp, err := c.do(ctx, http.MethodPost, e.addr, r.path, body, nil)
			if err != nil {
				t.Fatalf("%s at %d: %v", r.path, n, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			over := n > r.limit
			if got := strings.Contains(string(b), r.refused); got != over {
				t.Fatalf("%s at %d bytes (limit %d): %d %q, refused=%v want %v", r.path, n, r.limit, resp.StatusCode, b, got, over)
			}
			if over && resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s over the limit: %d", r.path, resp.StatusCode)
			}
		}
	}
}
