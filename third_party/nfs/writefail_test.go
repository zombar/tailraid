package nfs

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// brokenWrites is a connection that delivers one request frame, then blocks reading until
// closed, and fails every write: a socket whose send side broke while its receive side
// still waits.
type brokenWrites struct {
	net.Conn // only for the methods the server does not call
	mu       sync.Mutex
	frame    []byte
	closed   chan struct{}
	once     sync.Once
}

func (b *brokenWrites) Read(p []byte) (int, error) {
	b.mu.Lock()
	if len(b.frame) > 0 {
		n := copy(p, b.frame)
		b.frame = b.frame[n:]
		b.mu.Unlock()
		return n, nil
	}
	b.mu.Unlock()
	<-b.closed
	return 0, net.ErrClosed
}
func (b *brokenWrites) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (b *brokenWrites) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}
func (b *brokenWrites) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}
func (b *brokenWrites) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2} }

// A reply the socket refuses closes the connection: the reader and every handler waiting to
// hand over its reply stop. Before, the reply writer returned alone, the handlers blocked
// on it for good and the reader went on accepting requests until every slot was held by
// one: the connection hung with nothing to close it.
// Invariant: I-269
func TestConn_WriteFailure_ClosesTheConnection(t *testing.T) {
	bw := &brokenWrites{frame: frame(1, testProg, 1, 2, 0), closed: make(chan struct{})}
	c := &conn{Server: &Server{}, Conn: bw}
	done := make(chan struct{})
	go func() { c.serve(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = bw.Close() // let the test end
		<-done
		t.Fatal("a connection whose replies cannot be written stayed open")
	}
}
