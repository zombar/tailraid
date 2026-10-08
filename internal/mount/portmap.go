package mount

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

// Portmap is the ONC RPC portmapper v2 stub Windows' mount.exe needs (docs/12 §2): it maps
// NFS (100003 v3) and MOUNT (100005 v3) over TCP to the gateway port and nothing else.
type Portmap struct {
	Port uint32 // the NFS gateway port (2049 on Windows)
	Log  *slog.Logger
}

const (
	pmapProg    = 100000
	pmapVers    = 2
	progNFS     = 100003
	progMount   = 100005
	protoTCP    = 6
	procNull    = 0
	procGetport = 3
	procDump    = 4

	msgCall, msgReply         = 0, 1
	replyAccepted             = 0
	acceptSuccess             = 0
	acceptProcUnavail         = 3
	acceptProgMismatch        = 2
	maxPmapMsg                = 4096
	rpcVersion         uint32 = 2
)

// ErrNotLoopback is returned for a bind address off 127.0.0.1 / ::1.
var ErrNotLoopback = errors.New("portmap: refusing to listen on a non-loopback address")

// Serve answers on TCP and UDP at addr (loopback only) until ctx is done.
func (p *Portmap) Serve(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("portmap: address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return ErrNotLoopback
	}
	tl, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("portmap: tcp %s: %w", addr, err)
	}
	ul, err := net.ListenPacket("udp", addr)
	if err != nil {
		_ = tl.Close()
		return fmt.Errorf("portmap: udp %s: %w", addr, err)
	}
	return p.serve(ctx, tl, ul)
}

func (p *Portmap) serve(ctx context.Context, tl net.Listener, ul net.PacketConn) error {
	if p.Log == nil {
		p.Log = slog.Default()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go p.serveTCP(c)
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, maxPmapMsg)
		for {
			n, from, err := ul.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := p.handle(buf[:n]); reply != nil {
				_, _ = ul.WriteTo(reply, from)
			}
		}
	}()
	<-ctx.Done()
	_ = tl.Close()
	_ = ul.Close()
	wg.Wait()
	return nil
}

// serveTCP speaks RPC record marking: a 4-byte header (last-fragment bit | length).
func (p *Portmap) serveTCP(c net.Conn) {
	defer c.Close()
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:]) & 0x7fffffff
		if n > maxPmapMsg {
			return
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		reply := p.handle(msg)
		if reply == nil {
			return
		}
		binary.BigEndian.PutUint32(hdr[:], 0x80000000|uint32(len(reply)))
		if _, err := c.Write(append(hdr[:], reply...)); err != nil {
			return
		}
	}
}

// handle decodes one RPC call and returns the reply body, or nil for garbage.
func (p *Portmap) handle(msg []byte) []byte {
	r := xdrReader{b: msg}
	xid, mtype, rpcvers := r.u32(), r.u32(), r.u32()
	prog, vers, proc := r.u32(), r.u32(), r.u32()
	// credentials and verifier: flavor, length, body (skipped)
	for i := 0; i < 2; i++ {
		r.u32()
		r.skip(r.u32())
	}
	if r.err != nil || mtype != msgCall || rpcvers != rpcVersion {
		return nil
	}
	w := &xdrWriter{}
	w.u32(xid).u32(msgReply).u32(replyAccepted).u32(0).u32(0) // verifier AUTH_NULL
	if prog != pmapProg || vers != pmapVers {
		return w.u32(acceptProgMismatch).u32(pmapVers).u32(pmapVers).bytes()
	}
	switch proc {
	case procNull:
		return w.u32(acceptSuccess).bytes()
	case procGetport:
		mprog, mvers, mprot := r.u32(), r.u32(), r.u32()
		port := uint32(0)
		if r.err == nil && (mprog == progNFS || mprog == progMount) && mvers == 3 && mprot == protoTCP {
			port = p.Port
		}
		p.Log.Debug("portmap getport", "prog", mprog, "vers", mvers, "prot", mprot, "port", port)
		return w.u32(acceptSuccess).u32(port).bytes()
	case procDump:
		w.u32(acceptSuccess)
		for _, prog := range []uint32{progNFS, progMount} {
			w.u32(1).u32(prog).u32(3).u32(protoTCP).u32(p.Port) // "value follows" then the mapping
		}
		return w.u32(0).bytes()
	default:
		return w.u32(acceptProcUnavail).bytes()
	}
}

type xdrReader struct {
	b   []byte
	err error
}

func (r *xdrReader) u32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *xdrReader) skip(n uint32) {
	padded := (n + 3) &^ 3
	if r.err != nil || uint32(len(r.b)) < padded {
		r.err = io.ErrUnexpectedEOF
		return
	}
	r.b = r.b[padded:]
}

type xdrWriter struct{ b []byte }

func (w *xdrWriter) u32(v uint32) *xdrWriter { w.b = binary.BigEndian.AppendUint32(w.b, v); return w }
func (w *xdrWriter) bytes() []byte           { return w.b }
