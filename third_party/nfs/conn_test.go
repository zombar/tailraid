package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// tailraid: the connection serves requests concurrently (docs/07, I-167). These tests
// speak raw ONC RPC to a Server with a private program: procedure 1 blocks until the
// test releases it, procedure 2 answers at once.

const testProg = 400_000_000

var block = struct {
	mu      sync.Mutex
	release chan struct{}
	entered atomic.Int32
	arrived chan struct{} // one token per request that entered the blocking procedure
}{release: make(chan struct{}), arrived: make(chan struct{}, 1024)}

// awaitEntered waits for n more requests to enter the blocking procedure.
func awaitEntered(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-block.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d requests entered", block.entered.Load())
		}
	}
}

func init() {
	_ = RegisterMessageHandler(testProg, 1, func(ctx context.Context, w *response, _ Handler) error {
		block.entered.Add(1)
		block.arrived <- struct{}{}
		block.mu.Lock()
		release := block.release
		block.mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return w.Write([]byte{})
	})
	_ = RegisterMessageHandler(testProg, 2, func(_ context.Context, w *response, _ Handler) error {
		return w.Write([]byte{})
	})
}

// arm gives the blocking procedure a fresh gate and returns its release.
func arm() func() {
	ch := make(chan struct{})
	block.mu.Lock()
	block.release = ch
	block.entered.Store(0)
	block.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func serveTCP(t *testing.T) net.Conn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = (&Server{}).Serve(l); close(done) }()
	t.Cleanup(func() { _ = l.Close(); <-done })
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// frame is one record-marked RPC call of prog/vers/proc with pad bytes after the header.
func frame(xid, prog, vers, proc uint32, pad int) []byte {
	var body bytes.Buffer
	_ = xdr.Write(&body, xid)
	_ = xdr.Write(&body, uint32(0)) // call
	_ = xdr.Write(&body, rpc.Header{Rpcvers: 2, Prog: prog, Vers: vers, Proc: proc, Cred: rpc.AuthNull, Verf: rpc.AuthNull})
	body.Write(make([]byte, pad))
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(body.Len())|1<<31)
	out.Write(body.Bytes())
	return out.Bytes()
}

// headerLen is the bytes of frame(…, 0) after the record mark: xid, type and the header.
var headerLen = len(frame(0, 0, 0, 0, 0)) - 4

// readReply returns the XID of the next reply on c.
func readReply(c net.Conn) (uint32, error) {
	var mark [4]byte
	if _, err := io.ReadFull(c, mark[:]); err != nil {
		return 0, err
	}
	n := binary.BigEndian.Uint32(mark[:]) &^ (1 << 31)
	body := make([]byte, n)
	if _, err := io.ReadFull(c, body); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(body[:4]), nil
}

// A request that blocks does not hold the connection: the request sent after it is
// answered first, and the blocked one once released.
// Invariant: I-167
func TestConn_RepliesInCompletionOrder(t *testing.T) {
	release := arm()
	defer release()
	c := serveTCP(t)
	if _, err := c.Write(append(frame(1, testProg, 1, 1, 0), frame(2, testProg, 1, 2, 0)...)); err != nil {
		t.Fatal(err)
	}
	if xid, err := readReply(c); err != nil || xid != 2 {
		t.Fatalf("first reply xid %d err %v, want 2 (the unblocked request)", xid, err)
	}
	release()
	if xid, err := readReply(c); err != nil || xid != 1 {
		t.Fatalf("second reply xid %d err %v, want 1", xid, err)
	}
}

// At most InFlight requests of one connection run at once: the next one is not even
// read from the socket until a slot frees, and runs once one does.
// Invariant: I-167
func TestConn_InFlightBound(t *testing.T) {
	release := arm()
	defer release()
	c := serveTCP(t)
	var all []byte
	for i := 1; i <= InFlight+1; i++ {
		all = append(all, frame(uint32(i), testProg, 1, 1, 0)...)
	}
	all = append(all, frame(99, testProg, 1, 2, 0)...)
	if _, err := c.Write(all); err != nil {
		t.Fatal(err)
	}
	awaitEntered(t, InFlight)
	// the unblocked request behind them waits for a slot: no reply within a bounded wait,
	// and no further request entered
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if xid, err := readReply(c); err == nil {
		t.Fatalf("reply %d arrived while every slot was blocked", xid)
	} else if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read: %v", err)
	}
	if n := block.entered.Load(); n != InFlight {
		t.Fatalf("%d requests entered, want %d", n, InFlight)
	}
	_ = c.SetReadDeadline(time.Time{})
	release()
	seen := map[uint32]bool{}
	for range InFlight + 2 {
		xid, err := readReply(c)
		if err != nil {
			t.Fatal(err)
		}
		seen[xid] = true
	}
	if len(seen) != InFlight+2 || !seen[99] || !seen[InFlight+1] {
		t.Fatalf("replies %v", seen)
	}
	if n := block.entered.Load(); n != InFlight+1 {
		t.Fatalf("%d requests entered in all, want %d", n, InFlight+1)
	}
}

// A frame is read whole before its handler runs (a padded NULL still answers, and the
// frame behind it parses), up to MaxFrame; one byte more closes the connection.
// Invariant: I-167
func TestConn_ReadsWholeFrame_CapsIt(t *testing.T) {
	for _, size := range []int{headerLen, MaxFrame - 1, MaxFrame, MaxFrame + 1} {
		c := serveTCP(t)
		pad := size - headerLen
		two := append(frame(7, nfsServiceID, 3, uint32(NFSProcedureNull), pad), frame(8, nfsServiceID, 3, uint32(NFSProcedureNull), 0)...)
		if _, err := c.Write(two); err != nil && size <= MaxFrame {
			t.Fatalf("size %d: write: %v", size, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		xid, err := readReply(c)
		if size > MaxFrame {
			if err == nil {
				t.Fatalf("size %d: answered with xid %d, want the connection closed", size, xid)
			}
			continue
		}
		if err != nil {
			t.Fatalf("size %d: first reply: %v", size, err)
		}
		xid2, err := readReply(c)
		if err != nil {
			t.Fatalf("size %d: second reply: %v", size, err)
		}
		if (xid != 7 || xid2 != 8) && (xid != 8 || xid2 != 7) { // served concurrently: either order
			t.Fatalf("size %d: replies %d and %d, want 7 and 8", size, xid, xid2)
		}
	}
}
