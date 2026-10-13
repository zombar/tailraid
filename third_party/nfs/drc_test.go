package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// drcProc is a test procedure the duplicate request cache covers: it counts its runs, and
// waits at drcGate while one is set, as a WRITE waits at a cap.
const drcProc = 3

var (
	drcRuns atomic.Int32
	drcGate atomic.Pointer[chan struct{}]
	drcIn   = make(chan struct{}, 16)
)

func init() {
	_ = RegisterMessageHandler(testProg, drcProc, func(_ context.Context, w *response, _ Handler) error {
		n := drcRuns.Add(1)
		drcIn <- struct{}{}
		if g := drcGate.Load(); g != nil {
			<-*g // as a write waiting at a cap: the connection closing does not stop it
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n)) // which run answered
		return w.Write(b[:])
	})
	cached[testProg] = map[uint32]bool{drcProc: true}
}

// serveTCPDial serves one Server and returns a dialer of fresh connections to it: a
// client that reconnects comes back on another one.
func serveTCPDial(t *testing.T) func() net.Conn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = (&Server{}).Serve(l); close(done) }()
	t.Cleanup(func() { _ = l.Close(); <-done })
	return func() net.Conn {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

// replyBody is the reply's last four bytes: the run that answered.
func replyRun(t *testing.T, c net.Conn) uint32 {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var mark [4]byte
	if _, err := io.ReadFull(c, mark[:]); err != nil {
		t.Fatalf("no reply: %v", err)
	}
	b := make([]byte, binary.BigEndian.Uint32(mark[:])&^(1<<31))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return binary.BigEndian.Uint32(b[len(b)-4:])
}

// A request retransmitted on a new connection (the client reconnected) is answered from
// the duplicate request cache: once its first run finished, with that run's reply; while
// its first run is still under way, after it, with its reply; it never runs twice. Before,
// the copy ran again, alongside or after the original: a REMOVE answered NOENT for the
// file it had removed, and a WRITE still waiting on the old connection could land after
// the client's next WRITE of the same range. A different request under the same xid runs.
// Invariant: I-260
func TestDRC_RetransmissionRunsOnce(t *testing.T) {
	dial := serveTCPDial(t)
	drcRuns.Store(0)

	// finished: the copy gets the first run's reply
	a := dial()
	req := frame(41, testProg, 1, drcProc, 8)
	_, _ = a.Write(req)
	<-drcIn
	if run := replyRun(t, a); run != 1 {
		t.Fatalf("first reply from run %d", run)
	}
	b := dial()
	_, _ = b.Write(req)
	if run := replyRun(t, b); run != 1 || drcRuns.Load() != 1 {
		t.Fatalf("the retransmission was answered by run %d after %d runs, want run 1 of 1", run, drcRuns.Load())
	}

	// under way: the copy waits for the original and gets its reply
	gate := make(chan struct{})
	drcGate.Store(&gate)
	defer drcGate.Store(nil)
	c := dial()
	req2 := frame(42, testProg, 1, drcProc, 8)
	_, _ = c.Write(req2)
	<-drcIn // the original is in the handler, held at the gate
	_ = c.Close()
	d := dial()
	_, _ = d.Write(req2)
	answered := make(chan uint32, 1)
	go func() { answered <- replyRun(t, d) }()
	select {
	case run := <-answered:
		t.Fatalf("the retransmission was answered (run %d) while its original was still running", run)
	case <-drcIn:
		t.Fatal("the retransmission ran alongside its original")
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	if run := <-answered; run != 2 || drcRuns.Load() != 2 {
		t.Fatalf("the retransmission got run %d after %d runs, want the original's run 2 of 2", run, drcRuns.Load())
	}
	drcGate.Store(nil)

	// the same xid with another body is another request
	e := dial()
	_, _ = e.Write(frame(41, testProg, 1, drcProc, 12))
	<-drcIn
	if run := replyRun(t, e); run != 3 {
		t.Fatalf("a different request under a cached xid answered from run %d, want a run of its own (3)", run)
	}
}

// The cache keeps at most DRCEntries replies, dropping the oldest finished first; a
// procedure it does not cover (a READ, a GETATTR: safe to run again) is never cached.
// Invariant: I-260
func TestDRC_Bounded_CoversOnlyChanges(t *testing.T) {
	for _, n := range []int{0, 1, DRCEntries - 1, DRCEntries, DRCEntries + 1} {
		var d drc
		for i := range n {
			k := drcKey{xid: uint32(i)}
			d.begin(k)
			d.finish(k, []byte{1})
		}
		if got := d.len(); got != min(n, DRCEntries) {
			t.Errorf("%d replies cached: the cache holds %d, want %d", n, got, min(n, DRCEntries))
		}
	}
	for _, proc := range []NFSProcedure{NFSProcedureRead, NFSProcedureGetAttr, NFSProcedureLookup, NFSProcedureReadDirPlus, NFSProcedureNull} {
		if cached[nfsServiceID][uint32(proc)] {
			t.Errorf("%v is cached; it changes nothing and may run again", proc)
		}
	}
	for _, proc := range []NFSProcedure{NFSProcedureWrite, NFSProcedureCreate, NFSProcedureRemove, NFSProcedureRename, NFSProcedureMkDir, NFSProcedureSetAttr} {
		if !cached[nfsServiceID][uint32(proc)] {
			t.Errorf("%v is not cached; it must not run twice", proc)
		}
	}
}
