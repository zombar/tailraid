package smb

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// gatedReader is a positional handle whose ReadAt waits for the test to open its gate, as
// a driver's read waits for shards from the network.
type gatedReader struct {
	data    []byte
	gate    chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	closed  bool
	once    sync.Once
}

// open lets every waiting and future ReadAt through.
func (f *gatedReader) open() { f.once.Do(func() { close(f.gate) }) }

func (f *gatedReader) ReadAt(p []byte, off int64) (int, error) {
	f.entered <- struct{}{}
	<-f.gate
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, net.ErrClosed // a read the handle was closed under
	}
	return copy(p, f.data[off:]), nil
}
func (f *gatedReader) Size() int64 { return int64(len(f.data)) }
func (f *gatedReader) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// concurrentFS is tinyFS declaring its positional reads safe alongside its other calls.
type concurrentFS struct{ *tinyFS }

func (concurrentFS) ConcurrentReads() bool { return true }

func readConn(t *testing.T, concurrent bool) (*conn, *openFile, *gatedReader, net.Conn) {
	t.Helper()
	tiny := &tinyFS{body: []byte("x")}
	var c *conn
	var of *openFile
	if concurrent {
		c, of = opened(t, concurrentFS{tiny}, "/file.txt", false)
	} else {
		c, of = opened(t, tiny, "/file.txt", false)
	}
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	c.nc = server
	gr := &gatedReader{data: []byte("0123456789"), gate: make(chan struct{}), entered: make(chan struct{}, 8)}
	of.f = gr
	t.Cleanup(gr.open) // a test that failed with a READ in the driver lets it go
	return c, of, gr, client
}

// dispatchWithin dispatches msg and fails if the read loop is held longer than a READ's
// grace allows: a READ waiting in the driver must not keep the loop.
func dispatchWithin(t *testing.T, c *conn, msg []byte) []byte {
	t.Helper()
	type res struct {
		out []byte
		err error
	}
	done := make(chan res, 1)
	go func() {
		c.alone = true
		out, err := c.dispatch(msg)
		c.alone = false
		done <- res{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.out
	case <-time.After(2 * time.Second):
		t.Fatal("a READ waiting in the driver held the read loop")
		return nil
	}
}

func readRequest(of *openFile, msgID uint64, off int64, length uint32) []byte {
	body := make([]byte, 49)
	binary.LittleEndian.PutUint16(body[0:], 49)
	binary.LittleEndian.PutUint32(body[4:], length)
	binary.LittleEndian.PutUint64(body[8:], uint64(off))
	copy(body[16:], of.id[:])
	msg := request(cmdRead, 1, body)
	binary.LittleEndian.PutUint64(msg[offMessageID:], msgID)
	return msg
}

// A READ still waiting in the driver after the grace is answered STATUS_PENDING, the
// read loop goes on, and the real reply carries the data under the same MessageId and
// AsyncId; with a driver whose reads are concurrent, the waiting READ holds no share
// lock, so a CREATE, CLOSE or rename on the share is not held behind it.
// Invariant: I-254
func TestRead_Async_InterimThenFinal_NoShareLock(t *testing.T) {
	c, of, gr, client := readConn(t, true)
	interimCh := make(chan []byte, 1)
	go func() { interimCh <- readReply(t, client) }()
	if out := dispatchWithin(t, c, readRequest(of, 9, 2, 4)); out != nil {
		t.Fatalf("the read loop got a reply to send for a READ answered later: status %#x", statusOf(t, out))
	}
	<-gr.entered
	interim := <-interimCh
	ih, err := parseHeader(interim)
	if err != nil {
		t.Fatal(err)
	}
	if ih.status != statusPending || ih.messageID != 9 {
		t.Fatalf("interim: status %#x for %d, want STATUS_PENDING for 9", ih.status, ih.messageID)
	}
	asyncID := binary.LittleEndian.Uint64(interim[offAsyncID:])
	// the loop is free
	if out, err := c.dispatch(request(cmdEcho, 1, []byte{4, 0, 0, 0})); err != nil || statusOf(t, out) != statusSuccess {
		t.Fatalf("an ECHO behind the waiting READ: %v", err)
	}
	// and the share is not locked by it
	locked := make(chan struct{})
	go func() { of.share.changing()(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("a change on the share waited for a READ in the driver")
	}
	finalCh := make(chan []byte, 1)
	go func() { finalCh <- readReply(t, client) }()
	gr.open()
	final := <-finalCh
	fh, err := parseHeader(final)
	if err != nil {
		t.Fatal(err)
	}
	if fh.status != statusSuccess || fh.messageID != 9 || binary.LittleEndian.Uint64(final[offAsyncID:]) != asyncID {
		t.Fatalf("final: status %#x for %d, want success for 9 under %d", fh.status, fh.messageID, asyncID)
	}
	if n := binary.LittleEndian.Uint32(final[headerLen+4:]); n != 4 || string(final[headerLen+16:headerLen+20]) != "2345" {
		t.Fatalf("final carries %d bytes %q, want 2345", n, final[headerLen+16:])
	}
}

// A driver that does not declare concurrent reads keeps the share's read lock around its
// ReadAt, as before: a change waits for the READ.
// Invariant: I-254
func TestRead_SerialDriver_KeepsTheShareLock(t *testing.T) {
	c, of, gr, client := readConn(t, false)
	go func() { // drain the interim and the final reply; the test may end before the last one
		for {
			if _, err := readFrame(client); err != nil {
				return
			}
		}
	}()
	dispatchWithin(t, c, readRequest(of, 9, 0, 4))
	<-gr.entered
	locked := make(chan struct{})
	go func() { of.share.changing()(); close(locked) }()
	select {
	case <-locked:
		t.Fatal("a change ran under a READ of a driver that promised nothing")
	case <-time.After(100 * time.Millisecond):
	}
	gr.open()
	<-locked
}

// A CLOSE waits for the READs its handle accepted, so the driver's handle is never closed
// under one; and a CANCEL of a waiting READ answers STATUS_CANCELLED, since a read
// changes nothing.
// Invariant: I-254
func TestRead_Async_CloseWaits_CancelCancels(t *testing.T) {
	c, of, gr, client := readConn(t, true)
	replies := make(chan []byte, 2)
	go func() { // the interim and the CANCELLED: the late bytes are dropped
		for range 2 {
			replies <- readReply(t, client)
		}
	}()
	dispatchWithin(t, c, readRequest(of, 9, 0, 4))
	<-gr.entered
	interim := <-replies
	asyncID := binary.LittleEndian.Uint64(interim[offAsyncID:])
	cancel := request(cmdCancel, 0, []byte{4, 0, 0, 0})
	binary.LittleEndian.PutUint32(cancel[offFlags:], flagAsyncCommand)
	binary.LittleEndian.PutUint64(cancel[offAsyncID:], asyncID)
	if _, err := c.dispatch(cancel); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, <-replies); st != statusCancelled {
		t.Fatalf("the cancelled READ was answered %#x, want STATUS_CANCELLED", st)
	}
	closed := make(chan struct{})
	go func() { c.closeOpen(of); close(closed) }()
	select {
	case <-closed:
		t.Fatal("the handle closed under a READ still in the driver")
	case <-time.After(100 * time.Millisecond):
	}
	gr.open()
	<-closed
	if !gr.closed {
		t.Fatal("the handle was never closed")
	}
}
