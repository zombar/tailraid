package mesh

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// natSim is a NAT between a device and the loopback network: the device's bind sends
// through the simulator's outside sockets and receives what they accept. A cone NAT has
// one outside socket and accepts any source (endpoint-independent mapping and
// filtering); a symmetric NAT opens one socket per destination and accepts only that
// destination, so a reflexive address learned through one peer is wrong for another.
type natSim struct {
	t         *testing.T
	symmetric bool

	mu     sync.Mutex
	socks  map[string]*net.UDPConn // by destination, "" for the cone's one socket
	inbox  chan natPkt
	closed chan struct{}
	wg     sync.WaitGroup
}

type natPkt struct {
	data []byte
	src  netip.AddrPort
}

func newNAT(t *testing.T, symmetric bool) *natSim {
	return &natSim{t: t, symmetric: symmetric, socks: map[string]*net.UDPConn{}, inbox: make(chan natPkt, 256), closed: make(chan struct{})}
}

// socketFor is the outside socket packets to dst leave from, opened on first use.
func (n *natSim) socketFor(dst string) *net.UDPConn {
	key := ""
	if n.symmetric {
		key = dst
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if s, ok := n.socks[key]; ok {
		return s
	}
	s, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		n.t.Fatal(err)
	}
	n.socks[key] = s
	n.wg.Add(1)
	go n.read(s, key)
	return s
}

func (n *natSim) read(s *net.UDPConn, only string) {
	defer n.wg.Done()
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	buf := make([]byte, 4096)
	for {
		c, src, err := s.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if only != "" && src.String() != only {
			continue // a symmetric NAT drops what its mapping did not send to
		}
		pkt := natPkt{data: append([]byte(nil), buf[:c]...), src: src}
		select {
		case n.inbox <- pkt:
		case <-closed:
			return
		}
	}
}

// Reflexive is the outside address a peer at dst sees for the inside device (the
// mapping is made if needed, as a STUN answer or a /health's seen_from would make it).
func (n *natSim) Reflexive(dst string) netip.AddrPort {
	return netip.MustParseAddrPort(n.socketFor(dst).LocalAddr().String())
}

// natBind is the inside device's conn.Bind.
type natBind struct{ n *natSim }

// Open pairs with Close any number of times, as the device closes and reopens its bind.
func (b natBind) Open(uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.n.mu.Lock()
	b.n.closed = make(chan struct{})
	closed := b.n.closed
	b.n.mu.Unlock()
	recv := func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		select {
		case <-closed:
			return 0, net.ErrClosed
		case p := <-b.n.inbox:
			sizes[0] = copy(packets[0], p.data)
			eps[0] = &conn.StdNetEndpoint{AddrPort: p.src}
			return 1, nil
		}
	}
	return []conn.ReceiveFunc{recv}, 1, nil
}

func (b natBind) Close() error {
	b.n.mu.Lock()
	select {
	case <-b.n.closed:
	default:
		close(b.n.closed)
	}
	for k, s := range b.n.socks {
		_ = s.Close()
		delete(b.n.socks, k)
	}
	b.n.mu.Unlock()
	b.n.wg.Wait()
	return nil
}

func (natBind) SetMark(uint32) error { return nil }
func (natBind) BatchSize() int       { return 1 }

func (natBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &conn.StdNetEndpoint{AddrPort: ap}, nil
}

func (b natBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	dst := ep.DstToString()
	s := b.n.socketFor(dst)
	ap := netip.MustParseAddrPort(dst)
	for _, buf := range bufs {
		if _, err := s.WriteToUDPAddrPort(buf, ap); err != nil {
			return err
		}
	}
	return nil
}

// natted is a node behind a simulated NAT.
func natted(t *testing.T, idx uint32, symmetric bool) (*node, *natSim) {
	t.Helper()
	return nattedWith(t, idx, symmetric, Options{})
}

// nattedWith is natted with options of its own (the cluster key for the relay tests).
func nattedWith(t *testing.T, idx uint32, symmetric bool, o Options) (*node, *natSim) {
	t.Helper()
	sim := newNAT(t, symmetric)
	o.Inner = natBind{sim}
	return newNode(t, idx, o), sim
}

// Invariant: I-323
func TestNAT_ConeToCone_Punches(t *testing.T) {
	a, natA := natted(t, 1, false)
	b, natB := natted(t, 2, false)
	// each learned the other's reflexive address through a third member
	third := "127.0.0.1:9"
	if err := a.m.SetPeers([]Peer{b.peer(natB.Reflexive(third))}); err != nil {
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(natA.Reflexive(third))}); err != nil {
		t.Fatal(err)
	}
	if !ping(t, a, b, 10*time.Second) || !ping(t, b, a, 10*time.Second) {
		t.Fatal("two cone NATs did not punch")
	}
}

// Invariant: I-323
func TestNAT_ConeToSymmetric_Punches(t *testing.T) {
	a, natA := natted(t, 1, false)
	b, natB := natted(t, 2, true)
	third := "127.0.0.1:9"
	stale := natB.Reflexive(third) // b's mapping toward a third party: wrong for a
	if err := a.m.SetPeers([]Peer{b.peer(stale)}); err != nil {
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(natA.Reflexive(third))}); err != nil {
		t.Fatal(err)
	}
	// b, behind the symmetric NAT, reaches a's cone mapping; a pins the source it sees
	if !ping(t, b, a, 10*time.Second) {
		t.Fatal("the symmetric side did not reach the cone side")
	}
	if !ping(t, a, b, 10*time.Second) {
		t.Fatal("the cone side did not answer on the pinned path")
	}
	st := statusOf(t, a, b.m.Key())
	if st.Fanout || st.Endpoint == stale || !st.Endpoint.IsValid() {
		t.Fatalf("a's pin on b: %+v (stale %s)", st, stale)
	}
}

// Invariant: I-323, I-328
func TestNAT_SymmetricToSymmetric_NeedsARelay(t *testing.T) {
	a, natA := natted(t, 1, true)
	b, natB := natted(t, 2, true)
	third := "127.0.0.1:9"
	if err := a.m.SetPeers([]Peer{b.peer(natB.Reflexive(third))}); err != nil {
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(natA.Reflexive(third))}); err != nil {
		t.Fatal(err)
	}
	if ping(t, a, b, 2*time.Second) || ping(t, b, a, 2*time.Second) {
		t.Fatal("two symmetric NATs punched without a relay")
	}
	if st := statusOf(t, a, b.m.Key()); !st.Fanout || st.Class != PathNone {
		t.Fatalf("a's view of b: %+v", st)
	}
}
