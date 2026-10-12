package smb

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// gatedFile is a positional handle whose WriteAt waits for the test to open its gate,
// as a driver's write waits for room in a cap.
type gatedFile struct {
	mu      sync.Mutex
	data    []byte
	gate    chan struct{}
	entered chan struct{}
	closed  bool
}

func (f *gatedFile) ReadAt(p []byte, off int64) (int, error) { return 0, nil }
func (f *gatedFile) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.data))
}
func (f *gatedFile) Truncate(int64) error { return nil }
func (f *gatedFile) Sync() error          { return nil }
func (f *gatedFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *gatedFile) WriteAt(p []byte, off int64) (int, error) {
	f.entered <- struct{}{}
	<-f.gate
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, net.ErrClosed // a write the handle was closed under
	}
	if end := int(off) + len(p); end > len(f.data) {
		f.data = append(f.data, make([]byte, end-len(f.data))...)
	}
	copy(f.data[off:], p)
	return len(p), nil
}

// asyncConn is a connection whose replies sent from another goroutine land on a pipe the
// test reads, with one handle open on a gatedFile.
func asyncConn(t *testing.T) (*conn, *openFile, *gatedFile, net.Conn) {
	t.Helper()
	c, of := opened(t, &tinyFS{body: []byte("x")}, "/file.txt", false)
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	c.nc = server
	gf := &gatedFile{gate: make(chan struct{}), entered: make(chan struct{}, 8)}
	of.f, of.w = gf, gf
	return c, of, gf, client
}

func writeRequest(of *openFile, msgID uint64, off int64, data []byte) []byte {
	body := make([]byte, 48)
	binary.LittleEndian.PutUint16(body[0:], 49)
	binary.LittleEndian.PutUint16(body[2:], uint16(headerLen+48))
	binary.LittleEndian.PutUint32(body[4:], uint32(len(data)))
	binary.LittleEndian.PutUint64(body[8:], uint64(off))
	copy(body[16:], of.id[:])
	msg := append(request(cmdWrite, 1, body), data...)
	binary.LittleEndian.PutUint64(msg[offMessageID:], msgID)
	return msg
}

// readReply reads one frame from the client end of the pipe, or fails after a while.
func readReply(t *testing.T, nc net.Conn) []byte {
	t.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := readFrame(nc)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	return b
}

// A WRITE still waiting in the driver after the grace is answered STATUS_PENDING at once
// with an AsyncId, the read loop goes on to the next request, and the real reply follows
// once the driver returns: same MessageId, same AsyncId, the async flag, no credits.
// Invariant: I-217
func TestWrite_Async_InterimThenFinal(t *testing.T) {
	c, of, gf, client := asyncConn(t)
	interimCh := make(chan []byte, 1)
	go func() { interimCh <- readReply(t, client) }()
	c.alone = true
	out, err := c.dispatch(writeRequest(of, 7, 3, []byte("hello")))
	c.alone = false
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("the read loop got a reply to send for a WRITE answered later: status %#x", statusOf(t, out))
	}
	<-gf.entered
	interim := <-interimCh
	ih, err := parseHeader(interim)
	if err != nil {
		t.Fatal(err)
	}
	if ih.status != statusPending || ih.messageID != 7 || ih.flags&flagAsyncCommand == 0 {
		t.Fatalf("interim: status %#x message %d flags %#x, want STATUS_PENDING for 7, async", ih.status, ih.messageID, ih.flags)
	}
	asyncID := binary.LittleEndian.Uint64(interim[offAsyncID:])
	if asyncID == 0 {
		t.Fatal("the interim carries no AsyncId")
	}
	// the loop is free: another request on the connection is answered meanwhile
	if out, err := c.dispatch(request(cmdEcho, 1, []byte{4, 0, 0, 0})); err != nil || statusOf(t, out) != statusSuccess {
		t.Fatalf("an ECHO behind the waiting WRITE: %v", err)
	}
	finalCh := make(chan []byte, 1)
	go func() { finalCh <- readReply(t, client) }()
	close(gf.gate)
	final := <-finalCh
	fh, err := parseHeader(final)
	if err != nil {
		t.Fatal(err)
	}
	if fh.status != statusSuccess || fh.messageID != 7 || fh.flags&flagAsyncCommand == 0 ||
		binary.LittleEndian.Uint64(final[offAsyncID:]) != asyncID {
		t.Fatalf("final: status %#x message %d flags %#x async %d, want success for 7 under %d",
			fh.status, fh.messageID, fh.flags, binary.LittleEndian.Uint64(final[offAsyncID:]), asyncID)
	}
	if got := binary.LittleEndian.Uint16(final[offCredits:]); got != 0 {
		t.Errorf("the final reply grants %d credits; the interim granted them", got)
	}
	if got := binary.LittleEndian.Uint32(final[headerLen+4:]); got != 5 {
		t.Errorf("the final reply counts %d bytes written, want 5", got)
	}
	if string(gf.data[3:]) != "hello" {
		t.Errorf("the driver holds %q", gf.data)
	}
}

// A CLOSE of a handle answers only once every WRITE it accepted has been answered, and
// the driver's handle is closed after them, never under one.
// Invariant: I-217
func TestClose_WaitsForItsWrites(t *testing.T) {
	c, of, gf, client := asyncConn(t)
	replies := make(chan []byte, 4)
	go func() {
		for {
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			b, err := readFrame(client)
			if err != nil {
				close(replies)
				return
			}
			replies <- b
		}
	}()
	c.alone = true
	if out, err := c.dispatch(writeRequest(of, 9, 0, []byte("data"))); err != nil || out != nil {
		t.Fatalf("WRITE: %v, sync reply %v", err, out != nil)
	}
	c.alone = false
	<-gf.entered
	if h, _ := parseHeader(<-replies); h.status != statusPending {
		t.Fatalf("interim status %#x", h.status)
	}
	closeBody := make([]byte, 24)
	binary.LittleEndian.PutUint16(closeBody[0:], 24)
	copy(closeBody[8:], of.id[:])
	closed := make(chan []byte, 1)
	go func() {
		out, _ := c.dispatch(request(cmdClose, 1, closeBody))
		closed <- out
	}()
	select {
	case <-closed:
		t.Fatal("CLOSE answered while a WRITE of its handle was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(gf.gate)
	final, ok := <-replies
	if !ok {
		t.Fatal("no final reply to the WRITE")
	}
	if h, _ := parseHeader(final); h.status != statusSuccess || h.messageID != 9 {
		t.Fatalf("final: status %#x message %d", h.status, h.messageID)
	}
	if out := <-closed; statusOf(t, out) != statusSuccess {
		t.Fatalf("CLOSE answered %#x", statusOf(t, out))
	}
	if string(gf.data) != "data" || !gf.closed {
		t.Errorf("driver holds %q, closed %v", gf.data, gf.closed)
	}
}
