package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/rpc"
)

// ErrClusterMismatch is returned when a Raft peer presents a different cluster ID.
var ErrClusterMismatch = errors.New("cluster: raft peer belongs to a different cluster")

// tcpStream is a raft.StreamLayer over a plain TCP listener (raft's own TCP layer is
// not exported for wrapping).
type tcpStream struct {
	l         net.Listener
	advertise net.Addr
}

// Dial is raft's StreamLayer.Dial: no context reaches it, so the timeout is the only bound.
func (s *tcpStream) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	// from the listener's own address, the mesh one (docs/20 §1, I-317): a dial while the
	// interface is absent fails at once rather than leaving by another interface
	if ta, ok := s.l.Addr().(*net.TCPAddr); ok && ta.IP != nil && !ta.IP.IsUnspecified() {
		d.LocalAddr = &net.TCPAddr{IP: ta.IP}
	}
	return d.DialContext(context.Background(), "tcp", string(addr))
}
func (s *tcpStream) Accept() (net.Conn, error) { return s.l.Accept() }
func (s *tcpStream) Close() error              { return s.l.Close() }
func (s *tcpStream) Addr() net.Addr            { return s.advertise }

// PeerAuth decides whether a Raft connection from src may proceed to the handshake
// (docs/08 §Raft transport, I-92): the same member-by-IP plus WhoIs StableID rule the
// RPC server applies. rpc.WhoIsAuthorizer implements it.
type PeerAuth interface {
	Allow(ctx context.Context, src netip.Addr) (rpc.Principal, error)
}

// idStream wraps a StreamLayer with the member check on accept and a 16-byte cluster-ID
// handshake in both directions (docs/02, docs/08): a tailnet device that is not a member
// never reaches Raft, and two clusters that accidentally share a tailnet can never
// exchange Raft traffic.
type idStream struct {
	raft.StreamLayer
	id     [16]byte
	faults Faults       // nil: no rules
	auth   PeerAuth     // nil: accept any peer that passes the handshake (the in-process harness)
	log    *slog.Logger // nil: refusals are not logged

	once      sync.Once
	accepted  chan net.Conn // connections that passed the handshake
	closed    chan struct{}
	acceptMu  sync.Mutex
	acceptErr error
}

// Faults is the /dev/fault rule set (internal/dev.Faults): peers to refuse.
type Faults interface{ Blocked(addr string) bool }

// ErrHandshakeRefused is a dial the acceptor closed during the handshake: the dialer is
// not a member there (I-92) or belongs to another cluster (I-49); the acceptor, which
// reads first, never says which.
var ErrHandshakeRefused = errors.New("cluster: raft peer refused the handshake (not a member there, or another cluster)")

// errDropped is a dial refused by a fault rule.
var errDropped = errors.New("cluster: raft dial dropped by a fault rule")

// handshake exchanges cluster IDs. The dialer (initiator) writes first; the acceptor reads
// first, so an accepted peer learns nothing before it has shown its own ID.
func (s *idStream) handshake(c net.Conn, initiator bool) (net.Conn, error) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	write := func() error {
		if _, err := c.Write(s.id[:]); err != nil {
			_ = c.Close()
			return fmt.Errorf("cluster: handshake write: %w", err)
		}
		return nil
	}
	var peer [16]byte
	read := func() error {
		if _, err := io.ReadFull(c, peer[:]); err != nil {
			_ = c.Close()
			if initiator && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				return ErrHandshakeRefused // the acceptor closed without answering
			}
			return fmt.Errorf("cluster: handshake read: %w", err)
		}
		if !bytes.Equal(peer[:], s.id[:]) {
			_ = c.Close()
			return ErrClusterMismatch
		}
		return nil
	}
	steps := []func() error{read, write}
	if initiator {
		steps = []func() error{write, read}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

func (s *idStream) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	if s.faults != nil && s.faults.Blocked(string(addr)) {
		return nil, errDropped
	}
	c, err := s.StreamLayer.Dial(addr, timeout)
	if err != nil {
		return nil, err
	}
	hc, err := s.handshake(c, true)
	if err != nil {
		return nil, err
	}
	return s.guard(hc, string(addr)), nil
}

// guard wraps a Raft connection so that a drop rule naming its peer fails it: a rule
// cuts the links that were up when it landed (a leader's replication pipelines to its
// followers), not only new dials and new accepts (I-59). Without faults it is c itself.
func (s *idStream) guard(c net.Conn, peer string) net.Conn {
	if s.faults == nil {
		return c
	}
	return &guardedConn{Conn: c, peer: peer, faults: s.faults}
}

type guardedConn struct {
	net.Conn
	peer   string
	faults Faults
}

func (g *guardedConn) Read(p []byte) (int, error) {
	if g.faults.Blocked(g.peer) {
		_ = g.Close()
		return 0, errDropped
	}
	return g.Conn.Read(p)
}

func (g *guardedConn) Write(p []byte) (int, error) {
	if g.faults.Blocked(g.peer) {
		_ = g.Close()
		return 0, errDropped
	}
	return g.Conn.Write(p)
}

// allowed is the member check on an accepted connection (I-92): before any byte is read.
func (s *idStream) allowed(c net.Conn) bool {
	if s.auth == nil {
		return true
	}
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.auth.Allow(ctx, ap.Addr()); err != nil {
		if s.log != nil {
			s.log.Warn("raft connection refused: not a member", "from", ap.Addr(), "err", err)
		}
		return false
	}
	return true
}

// Accept hands out connections that passed the handshake. Handshakes run off the accept
// goroutine (each has a 5 s deadline), so a foreign or stalled peer never holds up the
// peers behind it, let alone wedges the listener.
func (s *idStream) Accept() (net.Conn, error) {
	s.once.Do(func() {
		s.accepted, s.closed = make(chan net.Conn), make(chan struct{})
		go s.pump()
	})
	select {
	case c := <-s.accepted:
		return c, nil
	case <-s.closed:
		s.acceptMu.Lock()
		defer s.acceptMu.Unlock()
		return nil, s.acceptErr
	}
}

func (s *idStream) pump() {
	for {
		c, err := s.StreamLayer.Accept()
		if err != nil {
			s.acceptMu.Lock()
			s.acceptErr = err
			s.acceptMu.Unlock()
			close(s.closed)
			return
		}
		if s.faults != nil && s.faults.Blocked(c.RemoteAddr().String()) {
			_ = c.Close()
			continue
		}
		go func() {
			if !s.allowed(c) {
				_ = c.Close()
				return
			}
			hc, err := s.handshake(c, false)
			if err != nil {
				return
			}
			select {
			case s.accepted <- s.guard(hc, c.RemoteAddr().String()):
			case <-s.closed:
				_ = hc.Close()
			}
		}()
	}
}

// NewTransport builds the Raft TCP transport with the member check on accept and the
// cluster-ID handshake; faults (may be nil) refuses peers under a /dev/fault rule in both
// directions; auth nil accepts any peer that passes the handshake (tests only: serve and
// join always pass the WhoIs authorizer).
func NewTransport(bind, advertise string, clusterID [16]byte, logOutput io.Writer, faults Faults, auth PeerAuth, log *slog.Logger) (raft.Transport, error) {
	var lc net.ListenConfig // raft owns the listener's lifetime through Transport.Close
	l, err := lc.Listen(context.Background(), "tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("cluster: raft listen %s: %w", bind, err)
	}
	adv := l.Addr()
	if advertise == "" {
		advertise = bind
	}
	if host, port, err := net.SplitHostPort(advertise); err == nil && host != "" && host != "0.0.0.0" {
		if tcp, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port)); err == nil {
			adv = tcp
		}
	}
	stream := &idStream{StreamLayer: &tcpStream{l: l, advertise: adv}, id: clusterID, faults: faults, auth: auth, log: log}
	return raft.NewNetworkTransport(stream, 3, 10*time.Second, logOutput), nil
}
