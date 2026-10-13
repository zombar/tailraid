package rpc

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

type recordRT struct {
	name string
	got  *[]string
}

func (r recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.got = append(*r.got, r.name+" "+req.URL.Path)
	return nil, errors.New("recorded")
}

// A mirror's objects and trees travel like shards (docs/16 §6), on the transport whose
// response headers are bounded: a host that went silent fails the read within that bound
// rather than hanging a /.snapshots read until TCP gives up. The control transport has no
// header bound (I-189) and had carried them.
// Invariant: I-241
func TestClient_MirrorGet_OnBoundedTransport(t *testing.T) {
	c := NewClient([16]byte{1}, func(meta.NodeIdx) (string, bool, bool) { return "127.0.0.1:1", false, true })
	if c.http.Transport.(*http.Transport).ResponseHeaderTimeout == 0 {
		t.Fatal("the shard transport has no header bound")
	}
	var got []string
	c.http.Transport = recordRT{"shard", &got}
	c.ctl.Transport = recordRT{"control", &got}
	_, _ = c.MirrorObject(context.Background(), 3, meta.Hash{1})
	_, _ = c.MirrorTree(context.Background(), 3, meta.Hash{2})
	if len(got) != 2 {
		t.Fatalf("requests %v", got)
	}
	for _, r := range got {
		if r[:5] != "shard" {
			t.Errorf("%s went on the control transport, which has no header bound", r)
		}
	}
}
