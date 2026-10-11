package rpc

import (
	"net/http"
	"testing"
)

// /v1/status passes fresh=1 to the status hook (the CLI and verify ask for a report built
// after the request) and a plain request as not fresh (docs/12 §8).
//
// Invariant: I-214
func TestStatus_FreshQuery(t *testing.T) {
	var asked []bool
	e := newEnvWith(t, func(o *Options) {
		o.Status = func(fresh bool) any { asked = append(asked, fresh); return map[string]bool{"fresh": fresh} }
	})
	c := e.client()
	for _, tc := range []struct {
		path string
		want bool
	}{{"/v1/status", false}, {"/v1/status?fresh=1", true}, {"/v1/status?fresh=0", false}} {
		body, err := c.Call(ctx, e.addr, http.MethodGet, tc.path, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if got := asked[len(asked)-1]; got != tc.want {
			t.Errorf("%s: the hook was asked fresh=%v (%s), want %v", tc.path, got, body, tc.want)
		}
	}
}
