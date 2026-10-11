package admin_test

import (
	"context"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
)

type pathCaller struct{ path string }

func (p *pathCaller) Call(_ context.Context, _, _, path string, _ []byte) ([]byte, error) {
	p.path = path
	return []byte(`{"node":"n"}`), nil
}

// FetchStatus, behind the CLI's status and verify's, asks for a report built after the
// request: what it prints follows the action before it (docs/12 §8).
//
// Invariant: I-214
func TestFetchStatus_AsksFresh(t *testing.T) {
	c := &pathCaller{}
	rep, err := admin.FetchStatus(context.Background(), c, "a:1")
	if err != nil || rep.Node != "n" {
		t.Fatalf("%+v %v", rep, err)
	}
	if c.path != "/v1/status?fresh=1" {
		t.Fatalf("asked %q, want /v1/status?fresh=1", c.path)
	}
}
