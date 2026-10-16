package nfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"sync"

	xdr2 "github.com/rasky/go-xdr/xdr2"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs/xdr"
)

var (
	// ErrInputInvalid is returned when input cannot be parsed
	ErrInputInvalid = errors.New("invalid input")
	// ErrAlreadySent is returned when writing a header/status multiple times
	ErrAlreadySent = errors.New("response already started")
)

// ResponseCode is a combination of accept_stat and reject_stat.
type ResponseCode uint32

// ResponseCode Codes: an accepted reply's accept_stat (RFC 5531 §9), or, from 1<<16 up, a
// denied reply's reject_stat plus 1<<16 (tailraid, I-245: the values were shifted by one).
const (
	ResponseCodeSuccess         ResponseCode = 0
	ResponseCodeProgUnavailable ResponseCode = 1
	ResponseCodeProgMismatch    ResponseCode = 2
	ResponseCodeProcUnavailable ResponseCode = 3
	ResponseCodeGarbageArgs     ResponseCode = 4
	ResponseCodeSystemErr       ResponseCode = 5
	ResponseCodeRPCMismatch     ResponseCode = denied + 0
	ResponseCodeAuthError       ResponseCode = denied + 1
)

// denied marks the codes of a MSG_DENIED reply.
const denied = 1 << 16

// versions are the program versions this server speaks; a call of another gets
// PROG_MISMATCH with the range (tailraid, I-245).
var versions = map[uint32]uint32{nfsServiceID: 3, mountServiceID: 3}

// tailraid: requests on one connection are served concurrently (docs/07, I-167).
//
// InFlight is the most requests one connection has in flight at once; MaxFrame is the
// largest request frame accepted, a WRITE of the advertised wtmax (1 MiB, nfs_onfsinfo.go)
// plus its RPC and NFS headers. A larger frame closes the connection.
//
// WriteSlots is the most of those a request that may wait on the write path holds, and
// QueuedWrites the most such requests waiting for a write slot, read but not started
// (tailraid, I-370): a WRITE can wait seconds at a cap, and without the split a copy's
// WRITEs took every slot and stopped the reader, so a READ behind them waited too.
const (
	InFlight     = 16
	WriteSlots   = 8
	QueuedWrites = 32
	MaxFrame     = 1<<20 + 8<<10
)

// waitsOnWrites reports a request that may wait on the write path: WRITE, held at a cap
// until the flusher frees room, and COMMIT, which waits for the flush.
var waitsOnWrites = func(h rpc.Header) bool {
	return h.Prog == nfsServiceID && (h.Proc == uint32(NFSProcedureWrite) || h.Proc == uint32(NFSProcedureCommit))
}

type conn struct {
	*Server
	writeSerializer chan []byte
	net.Conn
}

// serve reads request frames one after another and runs each on its own goroutine, at
// most InFlight at once; replies go through the connection's single writer in completion
// order (NFSv3 over TCP matches replies by XID, RFC 1813 §1.6). A request that may wait on
// the write path runs on at most WriteSlots of the slots, in the order read; one that
// finds them taken waits in a queue of at most QueuedWrites while the reader goes on, so
// the requests behind it are served (I-370). A frame that cannot be read or parsed, or a
// handler that fails at the transport level, closes the connection; the reader then
// returns and waits for the handlers still running.
func (c *conn) serve(ctx context.Context) {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.Close()
	c.writeSerializer = make(chan []byte, 1)
	go c.serializeWrites(connCtx, cancel)

	bio := bufio.NewReader(c.Conn)
	slots := make(chan struct{}, InFlight)
	writeSlots := make(chan struct{}, WriteSlots)
	writes := make(chan *response, QueuedWrites-1) // and one in the dispatcher's hand
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()      // runs before wg.Wait: a handler blocked on its reply stops
	defer close(writes) // runs first: the dispatcher's range ends
	run := func(w *response, release func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer release()
			c.run(connCtx, cancel, w)
		}()
	}
	wg.Add(1)
	go func() { // the writes, in the order read, each once a write slot and a slot are free
		defer wg.Done()
		for w := range writes {
			select {
			case writeSlots <- struct{}{}:
			case <-connCtx.Done():
				return
			}
			select {
			case slots <- struct{}{}:
			case <-connCtx.Done():
				return
			}
			run(w, func() { <-slots; <-writeSlots })
		}
	}()
	for {
		w, err := c.readRequest(connCtx, bio)
		if err != nil {
			// EOF, a closed socket or a frame the server refuses: the close is the
			// signal (not logged: the library logger is a global a test may be swapping)
			return
		}
		Log.Tracef("request: %v", w.req)
		if waitsOnWrites(w.req.Header) {
			select {
			case writes <- w: // the reader stops only while the queue is full
			case <-connCtx.Done():
				return
			}
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-connCtx.Done():
			return
		}
		run(w, func() { <-slots })
	}
}

// run handles one request and sends its reply; a failure at the transport level closes
// the connection.
func (c *conn) run(connCtx context.Context, cancel context.CancelFunc, w *response) {
	handle := c.handle
	if cached[w.req.Header.Prog][w.req.Header.Proc] {
		handle = c.handleOnce // a retransmission runs once (I-260)
	}
	if err := handle(connCtx, w); err != nil {
		Log.Errorf("error handling req: %v", err)
		// failure to handle at a level needing to close the connection.
		c.Close()
		cancel()
		return
	}
	if err := w.finish(connCtx); err != nil {
		Log.Errorf("error sending response: %v", err)
		c.Close()
		cancel()
	}
}

// serializeWrites writes the replies one after another. A reply the socket refuses ends
// the connection (tailraid, I-269): the socket is closed, so the reader stops, and the
// context cancelled, so every handler waiting to hand over its reply stops too. Before,
// the writer returned alone and those handlers waited for it for good.
func (c *conn) serializeWrites(ctx context.Context, cancel context.CancelFunc) {
	writer := bufio.NewWriter(c.Conn)
	fail := func() {
		_ = c.Close()
		cancel()
	}
	var fragmentBuf [4]byte
	var fragmentInt uint32
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.writeSerializer:
			if !ok {
				return
			}
			// prepend the fragmentation header
			fragmentInt = uint32(len(msg))
			fragmentInt |= (1 << 31)
			binary.BigEndian.PutUint32(fragmentBuf[:], fragmentInt)
			if _, err := writer.Write(fragmentBuf[:]); err != nil {
				fail()
				return
			}
			if _, err := writer.Write(msg); err != nil { // a short write comes with its error
				fail()
				return
			}
			if err := writer.Flush(); err != nil {
				fail()
				return
			}
		}
	}
}

// Handle a request. errors from this method indicate a failure to read or
// write on the network stream, and trigger a disconnection of the connection.
func (c *conn) handle(ctx context.Context, w *response) error {
	h := w.req.Header
	var refuse RPCError
	handler := c.Server.handlerFor(h.Prog, h.Proc)
	switch v, known := versions[h.Prog]; {
	case h.Rpcvers != 2:
		refuse = &RPCMismatchError{Low: 2, High: 2}
	case known && h.Vers != v:
		refuse = &ProgMismatchError{Low: v, High: v}
	case handler == nil && !c.Server.knows(h.Prog):
		refuse = &ProgUnavailableError{}
	case handler == nil:
		refuse = &ResponseCodeProcUnavailableError{}
	}
	if refuse != nil {
		Log.Errorf("Refusing %d.%d.%d (RPC %d): %v", h.Prog, h.Vers, h.Proc, h.Rpcvers, refuse)
		if err := w.drain(ctx); err != nil {
			return err
		}
		return c.err(ctx, w, refuse)
	}
	uh := c.Server.Handler
	if ch, ok := uh.(CredentialHandler); ok {
		uh = ch.WithCredential(w.req.Header.Cred) // the caller's own view (docs/21 §5)
	}
	appError := handler(ctx, w, uh)
	if drainErr := w.drain(ctx); drainErr != nil {
		return drainErr
	}
	if appError != nil && !w.responded {
		if err := c.err(ctx, w, appError); err != nil {
			return err
		}
	}
	if !w.responded {
		Log.Errorf("Handler did not indicate response status via writing or erroring")
		if err := c.err(ctx, w, &ResponseCodeSystemError{}); err != nil {
			return err
		}
	}
	return nil
}

func (c *conn) err(ctx context.Context, w *response, err error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	if w.err == nil {
		w.err = err
	}

	if w.responded {
		return nil
	}

	rpcErr := w.errorFmt(err)
	if errors.Is(err, xdr.ErrGarbage) { // arguments that do not decode (I-244, I-245)
		rpcErr = &GarbageArgsError{}
	}
	if writeErr := w.writeHeader(rpcErr.Code()); writeErr != nil {
		return writeErr
	}

	body, _ := rpcErr.MarshalBinary()
	return w.Write(body)
}

type request struct {
	xid uint32
	rpc.Header
	Body   io.Reader
	client string // the peer's address without its port (the duplicate request cache's key)
	sum    uint64 // FNV-1a of the whole frame
}

func (r *request) String() string {
	if r.Header.Prog == nfsServiceID {
		return fmt.Sprintf("RPC #%d (nfs.%s)", r.xid, NFSProcedure(r.Header.Proc))
	} else if r.Header.Prog == mountServiceID {
		return fmt.Sprintf("RPC #%d (mount.%s)", r.xid, MountProcedure(r.Header.Proc))
	}
	return fmt.Sprintf("RPC #%d (%d.%d)", r.xid, r.Header.Prog, r.Header.Proc)
}

type response struct {
	*conn
	writer    *bytes.Buffer
	responded bool
	err       error
	errorFmt  func(error) RPCError
	req       *request
}

func (w *response) writeXdrHeader() error {
	err := xdr.Write(w.writer, &w.req.xid)
	if err != nil {
		return err
	}
	respType := uint32(1)
	err = xdr.Write(w.writer, &respType)
	if err != nil {
		return err
	}
	return nil
}

func (w *response) writeHeader(code ResponseCode) error {
	if w.responded {
		return ErrAlreadySent
	}
	w.responded = true
	if err := w.writeXdrHeader(); err != nil {
		return err
	}

	if code >= denied { // MSG_DENIED: reject_stat, no verifier
		if err := xdr.Write(w.writer, rpc.MsgDenied); err != nil {
			return err
		}
		return xdr.Write(w.writer, uint32(code-denied))
	}
	if err := xdr.Write(w.writer, rpc.MsgAccepted); err != nil {
		return err
	}
	if err := xdr.Write(w.writer, &rpc.AuthNull); err != nil { // the verifier
		return err
	}
	return xdr.Write(w.writer, &code)
}

// Write a response to an xdr message
func (w *response) Write(dat []byte) error {
	if !w.responded {
		if err := w.writeHeader(ResponseCodeSuccess); err != nil {
			return err
		}
	}

	acc := 0
	for acc < len(dat) {
		n, err := w.writer.Write(dat[acc:])
		if err != nil {
			return err
		}
		acc += n
	}
	return nil
}

// drain discards what the handler left of the request frame. The frame was read whole
// before the handler ran (readRequest), so there is nothing left on the wire.
func (w *response) drain(context.Context) error {
	if _, ok := w.req.Body.(*bytes.Reader); ok {
		return nil
	}
	return io.ErrUnexpectedEOF
}

func (w *response) finish(ctx context.Context) error {
	select {
	case w.conn.writeSerializer <- w.writer.Bytes():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readRequest reads one request frame whole (at most MaxFrame bytes) and parses its RPC
// header; the body the handler decodes is the rest of the frame, in memory, so the next
// frame can be read while this one is served.
func (c *conn) readRequest(_ context.Context, reader *bufio.Reader) (w *response, err error) {
	fragment, err := xdr.ReadUint32(reader)
	if err != nil {
		if xdrErr := (*xdr2.UnmarshalError)(nil); errors.As(err, &xdrErr) {
			if xdrErr.Err == io.EOF {
				return nil, io.EOF
			}
		}
		return nil, err
	}
	if fragment&(1<<31) == 0 {
		Log.Warnf("Warning: haven't implemented fragment reconstruction.\n")
		return nil, ErrInputInvalid
	}
	reqLen := fragment - uint32(1<<31)
	if reqLen < 40 || reqLen > MaxFrame {
		return nil, ErrInputInvalid
	}
	frame := make([]byte, reqLen)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, err
	}
	r := bytes.NewReader(frame)

	xid, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	reqType, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if reqType != 0 { // 0 = request, 1 = response
		return nil, ErrInputInvalid
	}

	sum := fnv.New64a()
	_, _ = sum.Write(frame)
	client := c.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(client); err == nil {
		client = host
	}
	req := request{xid: xid, Header: rpc.Header{}, Body: r, client: client, sum: sum.Sum64()}
	if err = xdr.Read(r, &req.Header); err != nil {
		return nil, err
	}

	w = &response{
		conn:     c,
		req:      &req,
		errorFmt: basicErrorFormatter,
		// TODO: use a pool for these.
		writer: bytes.NewBuffer([]byte{}),
	}
	return w, nil
}
