package smbgw_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
	"github.com/zombar/tailraid/internal/vfs"
)

// Several files copied at once through one gateway, the way Explorer copies: each
// CREATEs the target, sets its end of file to the final length, WRITEs it front to back
// and closes, all on one connection, while the flusher's run loop flushes whatever it
// picks. Every file reads back byte for byte once the copies are done and flushed; a
// target whose length landed without its bytes is the loss the real cluster showed
// (2026-10-12: 18 files committed as holes during concurrent copies).
// Invariant: I-217
func TestSMB_ConcurrentCopies_EveryByteLands(t *testing.T) {
	cs := int(testharness.ChunkSize)
	for _, tc := range []struct {
		name   string
		copies int
	}{
		{"one", 1},
		{"two", 2},
		{"four", 4},
		{"eight", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := startGateway(t)
			fl := g.f.Flusher(func(c *vfs.Config) {
				// low thresholds put the loop in the flush-everything branch while the
				// copies run, as a large copy alongside does on a real gateway
				c.TotalDirtyHi = uint64(4 * cs)
				c.FileDirtyHi = uint64(2 * cs)
			})
			rctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan struct{})
			go func() { fl.Run(rctx); close(stopped) }()
			defer func() { cancel(); <-stopped }()

			seed, rng := testseed.New(t)
			payloads := make([][]byte, tc.copies)
			for i := range payloads {
				// a non-chunk-aligned length, at least a few chunks
				payloads[i] = testharness.Pattern(3*cs+rng.Intn(6*cs)+17, byte(i*31+1))
			}
			done := make(chan error, tc.copies)
			for i := range payloads {
				go func(i int) { done <- copyLikeExplorer(g, fmt.Sprintf("c%d.bin", i), payloads[i], cs/2) }(i)
			}
			finished := 0
			testharness.Eventually(t, func() bool {
				g.f.Clk.Advance(100 * time.Millisecond)
				for {
					select {
					case err := <-done:
						if err != nil {
							t.Fatalf("seed %d: copy: %v", seed, err)
						}
						finished++
						continue
					default:
					}
					return finished == tc.copies
				}
			}, "the copies never finished")
			cancel()
			<-stopped // Run flushes everything once on the way out
			for i, want := range payloads {
				name := fmt.Sprintf("/c%d.bin", i)
				got := g.f.ReadFile(g.f.InodeOf(g.f.FS(), name))
				if !bytes.Equal(got, want) {
					t.Fatalf("seed %d: %s: %d bytes, want %d, differs at %d", seed, name, len(got), len(want), testharness.FirstDiff(got, want))
				}
			}
		})
	}
}

// copyLikeExplorer is the copy engine's sequence for one file: CREATE, SET_INFO
// EndOfFile to the final length, sequential WRITEs of piece bytes, CLOSE.
func copyLikeExplorer(g *gateway, name string, payload []byte, piece int) error {
	fh, err := g.sh.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := fh.Truncate(int64(len(payload))); err != nil {
		_ = fh.Close()
		return fmt.Errorf("%s: set end of file: %w", name, err)
	}
	for off := 0; off < len(payload); off += piece {
		if _, err := fh.WriteAt(payload[off:min(off+piece, len(payload))], int64(off)); err != nil {
			_ = fh.Close()
			return fmt.Errorf("%s: write at %d: %w", name, off, err)
		}
	}
	return fh.Close()
}

// A WRITE that waits in the gateway (here at its file's WAL cap, with nothing flushing)
// holds up neither the connection nor the share: another file's CREATE, end of file and
// WRITEs on the same connection complete while it waits, and once the flusher drains the
// first file its WRITE completes too. Windows copies every file to a server over one
// connection, so a serial connection or a share-wide lock held across a write makes one
// stuck copy every copy's stall.
// Invariant: I-217
func TestSMB_WriteWaitingAtCap_DoesNotStallOtherFiles(t *testing.T) {
	cs := int64(testharness.ChunkSize)
	g := startGatewayWith(t, func(f *testharness.Fabric) { f.RestartOverlay(vfs.WithWALCap(2 * cs)) })
	big := testharness.Pattern(int(4*cs), 3)
	small := testharness.Pattern(int(cs+17), 7)
	aDone := make(chan error, 1)
	go func() { aDone <- copyLikeExplorer(g, "a.bin", big, int(cs)) }()
	// a's third WRITE meets the cap: wait until its WAL is there
	testharness.Eventually(t, func() bool {
		for _, d := range g.f.Ov.Dirty() {
			if d.WALBytes >= 2*cs {
				return true
			}
		}
		return false
	}, "a never reached its WAL cap")
	bDone := make(chan error, 1)
	go func() { bDone <- copyLikeExplorer(g, "b.bin", small, int(cs/2)) }()
	var stuck bool
	select {
	case err := <-bDone:
		must(t, err)
	case <-time.After(5 * time.Second):
		stuck = true // unblock a first: the cleanup cannot unmount behind its WRITE
	}
	select {
	case err := <-aDone:
		t.Fatalf("a finished while its WAL was at the cap: %v", err)
	default:
	}
	testharness.Eventually(t, func() bool {
		must(t, g.f.Flusher().FlushAll(ctx))
		select {
		case err := <-aDone:
			must(t, err)
			return true
		default:
			return false
		}
	}, "a never got past the cap")
	if stuck {
		must(t, <-bDone)
		t.Fatal("b's copy waited behind a's WRITE at its cap")
	}
	must(t, g.f.Flusher().FlushAll(ctx))
	for name, want := range map[string][]byte{"/a.bin": big, "/b.bin": small} {
		if got := g.f.ReadFile(g.f.InodeOf(g.f.FS(), name)); !bytes.Equal(got, want) {
			t.Fatalf("%s differs at %d", name, testharness.FirstDiff(got, want))
		}
	}
}

// A WRITE the vfs refuses is logged with the operation, the path and the error, and a
// second refusal within the minute is counted rather than logged.
// Invariant: I-218
func TestSMB_RefusedWrite_IsLogged(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	var buf bytes.Buffer
	a := smbgw.New(f.FS(), f.FSM).WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	must(t, a.WriteFile("/f.bin", nil, 0o644))
	fh, err := a.OpenFile("/f.bin")
	must(t, err)
	w := fh.(interface {
		WriteAt([]byte, int64) (int, error)
	})
	past := int64(meta.MaxFileSize(testharness.ChunkSize)) // EFBIG: no file reaches it
	for range 2 {
		if _, err := w.WriteAt([]byte("x"), past); err == nil {
			t.Fatal("a write past the largest file size succeeded")
		}
	}
	must(t, fh.Close())
	// a miss is not a refusal
	if _, err := a.Stat("/nothing"); err == nil {
		t.Fatal("stat of a missing name succeeded")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `msg="smb: write refused"`) || !strings.Contains(lines[0], "path=/f.bin") || !strings.Contains(lines[0], "err=") {
		t.Fatalf("log:\n%s", buf.String())
	}
}
