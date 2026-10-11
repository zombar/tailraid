package nfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	xdr2 "github.com/rasky/go-xdr/xdr2"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

var (
	// ErrInputInvalid is returned when input cannot be parsed
	ErrInputInvalid = errors.New("invalid input")
	// ErrAlreadySent is returned when writing a header/status multiple times
	ErrAlreadySent = errors.New("response already started")
)

// ResponseCode is a combination of accept_stat and reject_stat.
type ResponseCode uint32

// ResponseCode Codes
const (
	ResponseCodeSuccess ResponseCode = iota
	ResponseCodeProgUnavailable
	ResponseCodeProcUnavailable
	ResponseCodeGarbageArgs
	ResponseCodeSystemErr
	ResponseCodeRPCMismatch
	ResponseCodeAuthError
)

// tailraid: requests on one connection are served concurrently (docs/07, I-167).
//
// InFlight is the most requests one connection has in flight at once; MaxFrame is the
// largest request frame accepted, a WRITE of the advertised wtmax (1 MiB, nfs_onfsinfo.go)
// plus its RPC and NFS headers. A larger frame closes the connection.
const (
	InFlight = 16
	MaxFrame = 1<<20 + 8<<10
)

type conn struct {
	*Server
	writeSerializer chan []byte
	net.Conn
}

// serve reads request frames one after another and runs each on its own goroutine, at
// most InFlight at once; replies go through the connection's single writer in completion
// order (NFSv3 over TCP matches replies by XID, RFC 1813 §1.6). A frame that cannot be
// read or parsed, or a handler that fails at the transport level, closes the connection;
// the reader then returns and waits for the handlers still running.
func (c *conn) serve(ctx context.Context) {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.Close()
	c.writeSerializer = make(chan []byte, 1)
	go c.serializeWrites(connCtx)

	bio := bufio.NewReader(c.Conn)
	slots := make(chan struct{}, InFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel() // runs before wg.Wait: a handler blocked on its reply stops
	for {
		w, err := c.readRequest(connCtx, bio)
		if err != nil {
			// EOF, a closed socket or a frame the server refuses: the close is the
			// signal (not logged: the library logger is a global a test may be swapping)
			return
		}
		Log.Tracef("request: %v", w.req)
		select {
		case slots <- struct{}{}:
		case <-connCtx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := c.handle(connCtx, w); err != nil {
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
		}()
	}
}

func (c *conn) serializeWrites(ctx context.Context) {
	// todo: maybe don't need the extra buffer
	writer := bufio.NewWriter(c.Conn)
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
			n, err := writer.Write(fragmentBuf[:])
			if n < 4 || err != nil {
				return
			}
			n, err = writer.Write(msg)
			if err != nil {
				return
			}
			if n < len(msg) {
				panic("todo: ensure writes complete fully.")
			}
			if err = writer.Flush(); err != nil {
				return
			}
		}
	}
}

// Handle a request. errors from this method indicate a failure to read or
// write on the network stream, and trigger a disconnection of the connection.
func (c *conn) handle(ctx context.Context, w *response) error {
	handler := c.Server.handlerFor(w.req.Header.Prog, w.req.Header.Proc)
	if handler == nil {
		Log.Errorf("No handler for %d.%d", w.req.Header.Prog, w.req.Header.Proc)
		if err := w.drain(ctx); err != nil {
			return err
		}
		return c.err(ctx, w, &ResponseCodeProcUnavailableError{})
	}
	appError := handler(ctx, w, c.Server.Handler)
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
	if writeErr := w.writeHeader(rpcErr.Code()); writeErr != nil {
		return writeErr
	}

	body, _ := rpcErr.MarshalBinary()
	return w.Write(body)
}

type request struct {
	xid uint32
	rpc.Header
	Body io.Reader
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

	status := rpc.MsgAccepted
	if code == ResponseCodeAuthError || code == ResponseCodeRPCMismatch {
		status = rpc.MsgDenied
	}

	err := xdr.Write(w.writer, &status)
	if err != nil {
		return err
	}

	if status == rpc.MsgAccepted {
		// Write opaque_auth header.
		err = xdr.Write(w.writer, &rpc.AuthNull)
		if err != nil {
			return err
		}
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
		if xdrErr, ok := err.(*xdr2.UnmarshalError); ok {
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

	req := request{
		xid,
		rpc.Header{},
		r,
	}
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
