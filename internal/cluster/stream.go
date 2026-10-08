package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/raft"
)

// ErrClusterMismatch is returned when a Raft peer presents a different cluster ID.
var ErrClusterMismatch = errors.New("cluster: raft peer belongs to a different cluster")

// tcpStream is a raft.StreamLayer over a plain TCP listener (raft's own TCP layer is
// not exported for wrapping).
type tcpStream struct {
	l         net.Listener
	advertise net.Addr
}

func (s *tcpStream) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", string(addr), timeout)
}
func (s *tcpStream) Accept() (net.Conn, error) { return s.l.Accept() }
func (s *tcpStream) Close() error              { return s.l.Close() }
func (s *tcpStream) Addr() net.Addr            { return s.advertise }

// idStream wraps a StreamLayer with a 16-byte cluster-ID handshake in both directions
// (docs/02, docs/08): two clusters that accidentally share a tailnet can never exchange
// Raft traffic.
type idStream struct {
	raft.StreamLayer
	id     [16]byte
	faults Faults // nil: no rules
}

// Faults is the /dev/fault rule set (internal/dev.Faults): peers to refuse.
type Faults interface{ Blocked(addr string) bool }

// errDropped is a dial refused by a fault rule.
var errDropped = errors.New("cluster: raft dial dropped by a fault rule")

func (s *idStream) handshake(c net.Conn) (net.Conn, error) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(s.id[:]); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("cluster: handshake write: %w", err)
	}
	var peer [16]byte
	if _, err := io.ReadFull(c, peer[:]); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("cluster: handshake read: %w", err)
	}
	_ = c.SetDeadline(time.Time{})
	if !bytes.Equal(peer[:], s.id[:]) {
		_ = c.Close()
		return nil, ErrClusterMismatch
	}
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
	return s.handshake(c)
}

// Accept keeps accepting until a connection passes the handshake, so a foreign peer
// cannot wedge the listener.
func (s *idStream) Accept() (net.Conn, error) {
	for {
		c, err := s.StreamLayer.Accept()
		if err != nil {
			return nil, err
		}
		if s.faults != nil && s.faults.Blocked(c.RemoteAddr().String()) {
			_ = c.Close()
			continue
		}
		if hc, err := s.handshake(c); err == nil {
			return hc, nil
		}
	}
}

// NewTransport builds the Raft TCP transport with the cluster-ID handshake; faults (may
// be nil) refuses peers under a /dev/fault rule in both directions.
func NewTransport(bind string, clusterID [16]byte, logOutput io.Writer, faults Faults) (raft.Transport, error) {
	l, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("cluster: raft listen %s: %w", bind, err)
	}
	adv := l.Addr()
	if host, port, err := net.SplitHostPort(bind); err == nil && host != "" && host != "0.0.0.0" {
		if tcp, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port)); err == nil {
			adv = tcp
		}
	}
	stream := &idStream{StreamLayer: &tcpStream{l: l, advertise: adv}, id: clusterID, faults: faults}
	return raft.NewNetworkTransport(stream, 3, 10*time.Second, logOutput), nil
}
