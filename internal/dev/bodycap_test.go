package dev

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// /dev/fault reads at most 64 KiB: the bound and one under are applied (204), one over is 400.
//
// Invariant: I-116
func TestDev_FaultBodyCap(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	srv := httptest.NewServer(handler(meta.NewFSM(), nil, NewFaults(), log, func(int) {}))
	defer srv.Close()
	const limit = 64 << 10
	for _, n := range []int{limit - 1, limit, limit + 1} {
		head, tail := `{"drop":[],"pad":"`, `"}`
		body := head + strings.Repeat("a", n-len(head)-len(tail)) + tail
		resp, err := http.Post(srv.URL+"/dev/fault", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := http.StatusNoContent
		if n > limit {
			want = http.StatusBadRequest
		}
		if resp.StatusCode != want {
			t.Fatalf("%d-byte body: %d, want %d", n, resp.StatusCode, want)
		}
	}
}
