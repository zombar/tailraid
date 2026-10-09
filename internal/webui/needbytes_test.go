package webui_test

import (
	"testing"

	"github.com/zombar/tailraid/internal/webui"
)

// The free-space estimate widens k and m before adding them: at k+m = 256 the uint8 sum
// is 0, which made every upload fit.
//
// Invariant: I-81
func TestNeedBytes_KPlusMBoundary(t *testing.T) {
	for _, c := range []struct {
		k, m uint8
		want uint64
	}{
		{1, 0, 1000}, {2, 1, 1500}, {254, 1, 1000 * 255 / 254}, {255, 0, 1000}, {255, 1, 1000 * 256 / 255}, {1, 255, 256000}, {0, 7, 1000},
	} {
		if got := webui.NeedBytes(1000, c.k, c.m); got != c.want {
			t.Errorf("%d+%d: %d bytes, want %d", c.k, c.m, got, c.want)
		}
	}
}
