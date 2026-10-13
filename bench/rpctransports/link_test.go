package rpctransports

import (
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// profile is one path between two devices: the one-way delay, each direction's bandwidth
// per host (shared by every connection to that host), and a packet loss rate.
type profile struct {
	name  string
	delay time.Duration
	rate  float64 // bytes/s
	loss  float64 // per 1280-byte packet (tailscale0's MTU)
}

var profiles = []profile{
	{"lan", 250 * time.Microsecond, 100 << 20, 0},
	{"tailnet", 7500 * time.Microsecond, 25 << 20, 0},
	{"lossy", 7500 * time.Microsecond, 25 << 20, 0.002},
}

func (p profile) rtt() time.Duration { return 2 * p.delay }

// link serializes the bytes of every connection to one host in one direction.
type link struct {
	mu   sync.Mutex
	rate float64
	next time.Time
}

func (l *link) slot(n int) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	return l.next
}

const mtu = 1280

// tcpPenalty models what a lost segment costs a TCP connection: everything after it on
// that connection waits for the retransmission, about one RTT when fast retransmit
// recovers it and the 200 ms minimum RTO when it does not (a tenth of losses here). The
// model has no congestion window, so a loss costs only the stall.
func tcpPenalty(p profile, n int, rnd *rand.Rand) time.Duration {
	if p.loss == 0 {
		return 0
	}
	var d time.Duration
	for range (n + mtu - 1) / mtu {
		if rnd.Float64() < p.loss {
			if rnd.Float64() < 0.1 {
				d += 200 * time.Millisecond
			} else {
				d += p.rtt()
			}
		}
	}
	return d
}

type piece struct {
	b   []byte
	due time.Time
}

// sendBuffer bounds the bytes a connection has written and the link has not yet
// delivered: a TCP send buffer (or a QUIC sender's socket), so a writer feels backpressure
// and a small reply queues behind at most this much of a large one on the same connection.
const sendBuffer = 1 << 20

// slowConn delivers each write after the link carried it, the one-way delay passed and
// any retransmission stall of this connection ended, in order.
type slowConn struct {
	net.Conn
	p      profile
	link   *link
	rnd    *rand.Rand
	stall  time.Time // when the last stall ends: a later piece is never delivered before
	mu     sync.Mutex
	q      chan piece
	room   chan struct{} // one token per byte the send buffer has free, in 1 KiB units
	closed chan struct{}
	once   sync.Once
}

// takeRoom waits until the send buffer has room for n bytes, or is empty when n is more
// than it holds (a write larger than the buffer goes out as the buffer drains).
func takeRoom(room chan struct{}, closed chan struct{}, n int) bool {
	for range min((n+1023)/1024, sendBuffer/1024) {
		select {
		case <-room:
		case <-closed:
			return false
		}
	}
	return true
}

func giveRoom(room chan struct{}, n int) {
	for range min((n+1023)/1024, sendBuffer/1024) {
		select {
		case room <- struct{}{}:
		default:
		}
	}
}

func newRoom() chan struct{} {
	r := make(chan struct{}, sendBuffer/1024)
	for range sendBuffer / 1024 {
		r <- struct{}{}
	}
	return r
}

func newSlowConn(c net.Conn, p profile, l *link) *slowConn {
	s := &slowConn{Conn: c, p: p, link: l, rnd: rand.New(rand.NewPCG(rand.Uint64(), 1)), q: make(chan piece, 1<<14), room: newRoom(), closed: make(chan struct{})}
	go s.pump()
	return s
}

func (s *slowConn) Write(b []byte) (int, error) {
	if !takeRoom(s.room, s.closed, len(b)) {
		return 0, net.ErrClosed
	}
	due := s.link.slot(len(b)).Add(s.p.delay)
	s.mu.Lock()
	due = due.Add(tcpPenalty(s.p, len(b), s.rnd))
	if due.Before(s.stall) {
		due = s.stall
	}
	s.stall = due
	s.mu.Unlock()
	select {
	case s.q <- piece{b: append([]byte(nil), b...), due: due}:
		return len(b), nil
	case <-s.closed:
		return 0, net.ErrClosed
	}
}

func (s *slowConn) pump() {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		select {
		case pc := <-s.q:
			if d := time.Until(pc.due); d > 0 {
				t.Reset(d)
				select {
				case <-t.C:
				case <-s.closed:
					return
				}
			}
			if _, err := s.Conn.Write(pc.b); err != nil {
				return
			}
			giveRoom(s.room, len(pc.b))
		case <-s.closed:
			return
		}
	}
}

func (s *slowConn) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.Conn.Close()
}

// countingListener wraps every accepted connection and counts them.
type countingListener struct {
	net.Listener
	p     profile
	link  *link
	count *countdown
}

type countdown struct {
	mu sync.Mutex
	n  int64
}

func (c *countdown) add() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countdown) get() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.count.add()
	return newSlowConn(c, l.p, l.link), nil
}

// slowPacketConn is the datagram side: every packet takes its turn on the link, waits the
// one-way delay, and is lost with the profile's probability (QUIC sees real loss).
type slowPacketConn struct {
	net.PacketConn
	p      profile
	link   *link
	rnd    *rand.Rand
	mu     sync.Mutex
	q      chan dgram
	room   chan struct{}
	closed chan struct{}
	once   sync.Once
}

type dgram struct {
	b   []byte
	to  net.Addr
	due time.Time
}

func newSlowPacketConn(c net.PacketConn, p profile, l *link) *slowPacketConn {
	s := &slowPacketConn{PacketConn: c, p: p, link: l, rnd: rand.New(rand.NewPCG(rand.Uint64(), 2)), q: make(chan dgram, 1<<14), room: newRoom(), closed: make(chan struct{})}
	go s.pump()
	return s
}

func (s *slowPacketConn) WriteTo(b []byte, to net.Addr) (int, error) {
	if !takeRoom(s.room, s.closed, len(b)) {
		return 0, net.ErrClosed
	}
	due := s.link.slot(len(b)).Add(s.p.delay)
	s.mu.Lock()
	lost := s.p.loss > 0 && s.rnd.Float64() < s.p.loss
	s.mu.Unlock()
	if lost {
		giveRoom(s.room, len(b))
		return len(b), nil
	}
	select {
	case s.q <- dgram{b: append([]byte(nil), b...), to: to, due: due}:
		return len(b), nil
	case <-s.closed:
		return 0, net.ErrClosed
	}
}

func (s *slowPacketConn) pump() {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		select {
		case d := <-s.q:
			if w := time.Until(d.due); w > 0 {
				t.Reset(w)
				select {
				case <-t.C:
				case <-s.closed:
					return
				}
			}
			if _, err := s.PacketConn.WriteTo(d.b, d.to); err != nil {
				return
			}
			giveRoom(s.room, len(d.b))
		case <-s.closed:
			return
		}
	}
}

func (s *slowPacketConn) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.PacketConn.Close()
}
