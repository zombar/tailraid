package nfsgw_test

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	nfs "github.com/willscott/go-nfs"

	"github.com/zombar/tailraid/internal/nfsgw"
	"github.com/zombar/tailraid/internal/testharness"
)

// Stopping the gateway closes the connections it accepted, not only the listener: a
// client still connected gets EOF instead of a gateway whose flusher has stopped.
//
// Invariant: I-100
func TestServe_ClosesConnectionsOnShutdown(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	h := nfsgw.New(f.FS(), f.FSM)
	l, err := nfsgw.Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- nfsgw.Serve(ctx, l, h) }()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("the connection stayed open after shutdown (read %d bytes)", n)
	}
}

// The go-nfs logger glue reports a library "panic" as an error and never panics the
// gateway, and library errors reach the log at the error level.
func TestLogger_PanicIsAnErrorLine(t *testing.T) {
	nfsgw.UseLogger(slog.New(slog.DiscardHandler))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the logger panicked: %v", r)
		}
	}()
	nfs.Log.Panic("library gave up")
	nfs.Log.Panicf("library gave up on %d", 7)
	nfs.Log.Error("still here")
}
