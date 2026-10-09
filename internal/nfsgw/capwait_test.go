package nfsgw_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// A client that outruns the flusher is throttled, not failed: the WRITE that meets the
// WAL cap is held on the wire until a flush has committed, then answered FILE_SYNC, and
// every byte is there afterwards (docs/07 "Large copies and the caps").
// Invariant: I-79
func TestNFS_WriteAtCap_WaitsThenSucceeds(t *testing.T) {
	cs := int64(testharness.ChunkSize)
	g := startGatewayWith(t, func(f *testharness.Fabric) { f.RestartOverlay(vfs.WithWALCap(2 * cs)) })
	if _, err := g.tgt.Create("/big", 0o644); err != nil {
		t.Fatal(err)
	}
	payload := testharness.Pattern(int(4*cs), 5)
	done := make(chan error, 1)
	go func() {
		fh, err := g.tgt.OpenFile("/big", 0o644)
		if err != nil {
			done <- err
			return
		}
		for off := int64(0); off < int64(len(payload)); off += cs {
			if _, err := fh.Seek(off, io.SeekStart); err != nil {
				done <- err
				return
			}
			if _, err := fh.Write(payload[off : off+cs]); err != nil {
				done <- err
				return
			}
		}
		done <- fh.Close()
	}()
	// the third WRITE meets the cap and waits; flushing (as the run loop would at the
	// cap) frees the WAL and lets it through
	testharness.Eventually(t, func() bool {
		must(t, g.f.Flusher().FlushAll(ctx))
		g.f.Clk.Advance(250 * time.Millisecond)
		select {
		case err := <-done:
			must(t, err)
			return true
		default:
			return false
		}
	}, "writes never completed past the WAL cap")
	must(t, g.f.Flusher().FlushAll(ctx))
	if got := g.readAll("/big", len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, payload))
	}
}

// When the cluster has no room for the shards, the WRITE that meets the cap fails with
// NFS3ERR_NOSPC ("No space left on device"), not NFS3ERR_IO.
// Invariant: I-79
// Lifecycle: L-189
func TestNFS_WriteWhenFull_IsNOSPC(t *testing.T) {
	const nospc = 28
	g := startGatewayWith(t, func(f *testharness.Fabric) {
		f.RestartOverlay(vfs.WithWALCap(1000), vfs.WithCapWait(0))
		for n := meta.NodeIdx(1); n <= 3; n++ {
			f.SetFull(n, true)
		}
	})
	if _, err := g.tgt.Create("/f", 0o644); err != nil {
		t.Fatal(err)
	}
	g.writeAt("/f", 0, testharness.Pattern(900, 6)) // into the WAL, acknowledged
	if err := g.f.Flusher().FlushAll(ctx); err == nil {
		t.Fatal("flush succeeded against full holders")
	}
	g.writeAt("/f", 450, testharness.Pattern(900, 6)) // a read-modify-write at a non-zero offset: the WAL reaches its cap
	fh, err := g.tgt.OpenFile("/f", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fh.Write([]byte("x"))
	if got := wireStatus(err); got != nospc {
		t.Fatalf("WRITE at the cap behind a full cluster: status %d (%v), want NFS3ERR_NOSPC", got, err)
	}
}
