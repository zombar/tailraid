package rpc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// A control-plane call outlives the shard transport's response-header timeout: an admin
// op that runs a pass is answered when the pass is done, bounded by the caller's
// context alone, while a shard call to the same stalled peer still fails at the header
// timeout.
//
// Invariant: I-189
func TestClient_PassOpOutlivesHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scanned":1}`))
	}))
	defer hs.Close()
	addr := hs.Listener.Addr().String()
	c := NewClient([16]byte{1}, func(meta.NodeIdx) (string, bool, bool) { return addr, false, true })
	c.SetDisks(func(meta.NodeIdx) (string, bool) { return "d1", true }, nil)
	c.http.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := c.Admin(ctx, addr, AdminOp{Op: "repair_now"})
		done <- err
	}()
	// the shard call to the stalled peer gives up at the header timeout
	if _, err := c.Has(ctx, 1, chunk.ShardKey("k")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("shard HEAD against a stalled peer: %v, want unavailable at the header timeout", err)
	}
	select {
	case err := <-done:
		t.Fatalf("the pass op returned before the pass finished: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the pass op after the pass: %v", err)
	}
}

// Invariant: I-189
func TestAdminOp_IsPass_Table(t *testing.T) {
	for _, tc := range []struct {
		op   AdminOp
		want bool
	}{
		{AdminOp{Op: "repair_now"}, true},
		{AdminOp{Op: "gc_now"}, true},
		{AdminOp{Op: "check", Scrub: true}, true},
		{AdminOp{Op: "check"}, false},
		{AdminOp{Op: "trash_purge", All: true}, true},
		{AdminOp{Op: "trash_purge", Ino: 7}, false},
		{AdminOp{Op: "snapshot_restore", Mirror: "pike/m1", Name: "s", Path: "/"}, false}, // a job on the mirror's host, answered once recorded (docs/16 §6)
		{AdminOp{Op: "versions_revert", Mirror: "pike/m1", Name: "s", Path: "/f"}, true},
		{AdminOp{Op: "versions_copy", Mirror: "pike/m1", Name: "s", Path: "/f"}, true},
		{AdminOp{Op: "versions", Path: "/f"}, false},
		{AdminOp{Op: "set_policy"}, false},
		{AdminOp{Op: "fsck"}, false},
		{AdminOp{Op: "chunk", Chunk: "/f"}, false},
	} {
		if got := tc.op.IsPass(); got != tc.want {
			t.Errorf("%+v: pass %v, want %v", tc.op, got, tc.want)
		}
	}
}
