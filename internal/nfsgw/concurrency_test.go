package nfsgw_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// writeRange writes payload[off:off+n] to path in pieces of piece bytes through the
// client, the way a kernel client would, and reports the first error on done.
func (g *gateway) writeRange(path string, payload []byte, off, n, piece int64, done chan<- error) {
	fh, err := g.tgt.OpenFile(path, 0o644)
	if err != nil {
		done <- err
		return
	}
	for p := off; p < off+n; p += piece {
		if _, err := fh.Seek(p, io.SeekStart); err != nil {
			done <- err
			return
		}
		if _, err := fh.Write(payload[p : p+piece]); err != nil {
			done <- err
			return
		}
	}
	done <- fh.Close()
}

// A request that blocks on the vfs (a WRITE waiting at the WAL cap) holds neither its
// connection's other requests nor the mount: a GETATTR sent behind it is answered while
// it waits (the in-tree go-nfs copy dispatches a connection's requests concurrently).
// Invariant: I-167
func TestNFS_ConcurrentRPCsOnOneConnection(t *testing.T) {
	cs := int64(testharness.ChunkSize)
	g := startGatewayWith(t, func(f *testharness.Fabric) { f.RestartOverlay(vfs.WithWALCap(2 * cs)) })
	if _, err := g.tgt.Create("/big", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/other", 0o644); err != nil {
		t.Fatal(err)
	}
	payload := testharness.Pattern(int(3*cs), 7)
	pending := g.f.Clk.Pending()
	done := make(chan error, 1)
	go g.writeRange("/big", payload, 0, 3*cs, cs, done)
	// the third WRITE meets the cap and waits on the fake clock (CapWait)
	testharness.Eventually(t, func() bool { return g.f.Clk.Pending() > pending }, "the WRITE at the cap never waited")
	got := make(chan error, 1)
	go func() {
		_, err := g.tgt.Getattr("/other")
		got <- err
	}()
	select {
	case err := <-got:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("GETATTR queued behind the waiting WRITE")
	}
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

// A connection runs at most InFlight requests at once: with that many WRITEs waiting at
// the cap, the next request waits for a slot, and everything completes once the flusher
// frees the cap.
// Invariant: I-167
func TestNFS_InFlightBound(t *testing.T) {
	const inFlight = 16 // nfs.InFlight
	cs := int64(testharness.ChunkSize)
	g := startGatewayWith(t, func(f *testharness.Fabric) { f.RestartOverlay(vfs.WithWALCap(cs)) })
	if _, err := g.tgt.Create("/big", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/other", 0o644); err != nil {
		t.Fatal(err)
	}
	piece := cs / inFlight // the second chunk, in inFlight pieces, one WRITE each
	payload := testharness.Pattern(int(2*cs), 8)
	g.writeAt("/big", 0, payload[:cs]) // fills the WAL to its cap: every WRITE behind it waits
	pending := g.f.Clk.Pending()
	done := make(chan error, inFlight)
	for i := range int64(inFlight) {
		go g.writeRange("/big", payload, cs+i*piece, piece, piece, done)
	}
	testharness.Eventually(t, func() bool { return g.f.Clk.Pending() >= pending+inFlight }, "the WRITEs never all waited at the cap")
	got := make(chan error, 1)
	go func() {
		_, err := g.tgt.Getattr("/other")
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("GETATTR answered (%v) while every slot held a waiting WRITE", err)
	case <-time.After(300 * time.Millisecond):
	}
	finished := 0
	testharness.Eventually(t, func() bool {
		must(t, g.f.Flusher().FlushAll(ctx))
		g.f.Clk.Advance(250 * time.Millisecond)
		for {
			select {
			case err := <-done:
				must(t, err)
				finished++
			default:
				return finished == inFlight
			}
		}
	}, "writes never completed past the WAL cap")
	select {
	case err := <-got:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("GETATTR never answered after the slots freed")
	}
	must(t, g.f.Flusher().FlushAll(ctx))
	if got := g.readAll("/big", len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, payload))
	}
}

// FSINFO advertises the frame bound's payload, 1 MiB, as the largest transfer, so a
// client that honours it never sends a WRITE the gateway would refuse.
// Invariant: I-167
func TestNFS_FSInfo_AdvertisesFrameCap(t *testing.T) {
	g := startGateway(t)
	info, err := g.tgt.FSInfo()
	if err != nil {
		t.Fatal(err)
	}
	const want = 1 << 20
	if info.RTMax != want || info.WTMax != want || info.RTPref != want || info.WTPref != want {
		t.Fatalf("rtmax %d rtpref %d wtmax %d wtpref %d, want %d", info.RTMax, info.RTPref, info.WTMax, info.WTPref, want)
	}
}
